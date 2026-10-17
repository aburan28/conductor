// Package wireguard prepares static WireGuard configuration templates. It never
// generates or collects private keys and never changes host networking.
package wireguard

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxManifestSize = 1 << 20

type Node struct {
	Name      string `yaml:"name"`
	Address   string `yaml:"address"`
	PublicKey string `yaml:"public_key"`
}

type Gateway struct {
	Node       `yaml:",inline"`
	Endpoint   string `yaml:"endpoint"`
	ListenPort int    `yaml:"listen_port"`
}

type Manifest struct {
	Version int `yaml:"version"`
	Network struct {
		CIDR string `yaml:"cidr"`
	} `yaml:"network"`
	Gateway Gateway `yaml:"gateway"`
	Nodes   []Node  `yaml:"nodes"`
}

// Load accepts one bounded, strict YAML document. Secret keys, exit routes, and
// script hooks are intentionally absent from the format and rejected as unknown.
func Load(reader io.Reader) (*Manifest, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("read WireGuard manifest: %w", err)
	}
	if len(body) > maxManifestSize {
		return nil, errors.New("WireGuard manifest exceeds 1 MiB")
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(body)))
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode WireGuard manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("WireGuard manifest must contain exactly one YAML document")
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

var nodeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,14}$`)

func (m *Manifest) Validate() error {
	if m.Version != 1 {
		return errors.New("WireGuard manifest version must be 1")
	}
	overlay, err := netip.ParsePrefix(m.Network.CIDR)
	if err != nil || !overlay.Addr().Is4() || overlay != overlay.Masked() || overlay.Bits() > 30 || !privateOverlay(overlay) {
		return errors.New("network.cidr must be a canonical RFC1918 IPv4 subnet with room for gateway and workers")
	}
	if len(m.Nodes) == 0 || len(m.Nodes) > 4096 {
		return errors.New("nodes must contain between 1 and 4096 workers")
	}
	if err := gatewayEndpoint(m.Gateway.Endpoint); err != nil {
		return err
	}
	endpointHost, _, _ := net.SplitHostPort(m.Gateway.Endpoint)
	if endpointIP, err := netip.ParseAddr(endpointHost); err == nil && overlay.Contains(endpointIP) {
		return errors.New("gateway.endpoint must use an underlay address outside network.cidr")
	}
	if m.Gateway.ListenPort < 0 || m.Gateway.ListenPort > 65535 {
		return errors.New("gateway.listen_port must be 1..65535, or omitted to use the endpoint port")
	}
	names, addresses, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, node := range append([]Node{m.Gateway.Node}, m.Nodes...) {
		if !nodeName.MatchString(node.Name) {
			return fmt.Errorf("node name %q must be 1..15 lowercase letters/digits/hyphens, starting with a letter", node.Name)
		}
		address, err := netip.ParseAddr(node.Address)
		if err != nil || !overlay.Contains(address) || address == overlay.Addr() || broadcast(address, overlay) {
			return fmt.Errorf("node %s address must be a usable host in network.cidr", node.Name)
		}
		key, err := base64.StdEncoding.Strict().DecodeString(node.PublicKey)
		if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != node.PublicKey || allZero(key) {
			return fmt.Errorf("node %s public_key must be a nonzero canonical base64 WireGuard public key (32 bytes)", node.Name)
		}
		if names[node.Name] || addresses[node.Address] || keys[node.PublicKey] {
			return fmt.Errorf("node %s duplicates a name, address, or public key", node.Name)
		}
		names[node.Name], addresses[node.Address], keys[node.PublicKey] = true, true, true
	}
	return nil
}

func privateOverlay(prefix netip.Prefix) bool {
	for _, private := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		block := netip.MustParsePrefix(private)
		if prefix.Bits() >= block.Bits() && block.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func broadcast(address netip.Addr, prefix netip.Prefix) bool {
	bytes := address.As4()
	value := uint32(bytes[0])<<24 | uint32(bytes[1])<<16 | uint32(bytes[2])<<8 | uint32(bytes[3])
	hostMask := uint32(1)<<(32-prefix.Bits()) - 1
	return value&hostMask == hostMask
}

func allZero(key []byte) bool {
	for _, value := range key {
		if value != 0 {
			return false
		}
	}
	return true
}

func gatewayEndpoint(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" {
		return errors.New("gateway.endpoint must be a reachable host:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return errors.New("gateway.endpoint requires a numeric port between 1 and 65535")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if (!address.IsGlobalUnicast() && !address.IsLoopback()) || address.Zone() != "" {
			return errors.New("gateway.endpoint requires a unicast host")
		}
		return nil
	}
	if len(host) > 253 {
		return errors.New("gateway.endpoint hostname is too long")
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("gateway.endpoint requires a valid hostname")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' {
				return errors.New("gateway.endpoint requires a valid hostname")
			}
		}
	}
	return nil
}
