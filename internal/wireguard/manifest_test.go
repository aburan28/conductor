package wireguard

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

func publicKey(value byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
}

type failingManifestReader struct{ err error }

func (r failingManifestReader) Read([]byte) (int, error) { return 0, r.err }

func TestManifestReadAndDocumentBoundaries(t *testing.T) {
	sentinel := errors.New("inventory transport failed")
	if manifest, err := Load(io.MultiReader(strings.NewReader(validYAML()), failingManifestReader{sentinel})); manifest != nil || !errors.Is(err, sentinel) {
		t.Fatalf("partial read = %v, %v; want no inventory and wrapped read error", manifest, err)
	}
	// A valid document may occupy the entire budget, including comments.
	body := validYAML()
	body += "#" + strings.Repeat("x", maxManifestSize-len(body)-1)
	if _, err := Load(strings.NewReader(body)); err != nil {
		t.Fatalf("exactly 1 MiB manifest rejected: %v", err)
	}
	if _, err := Load(strings.NewReader(body + "x")); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversized valid document error = %v", err)
	}
	for name, body := range map[string]string{
		"empty":                     "",
		"malformed":                 "gateway: [",
		"empty second document":     validYAML() + "---\n",
		"malformed second document": validYAML() + "---\n[",
		"duplicate mapping key":     validYAML() + "version: 1\n",
		"worker hook":               strings.Replace(validYAML(), "  - name: worker-a", "  - post_up: echo unexpected\n    name: worker-a", 1),
		"worker route":              strings.Replace(validYAML(), "  - name: worker-a", "  - allowed_ips: 0.0.0.0/0\n    name: worker-a", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if manifest, err := Load(strings.NewReader(body)); err == nil || manifest != nil {
				t.Fatalf("invalid document = %v, %v", manifest, err)
			}
		})
	}
}

func TestManifestValidationBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement, message string }{
		{"malformed overlay", "10.88.0.0/24", "10.88.0.0/garbage", "network.cidr"},
		{"empty endpoint host", "gateway.internal:51820", ":51820", "gateway.endpoint"},
		{"zero endpoint port", "gateway.internal:51820", "gateway.internal:0", "numeric port"},
		{"padded endpoint port", "gateway.internal:51820", "gateway.internal:051820", "numeric port"},
		{"named endpoint port", "gateway.internal:51820", "gateway.internal:wireguard", "numeric port"},
		{"negative listen port", "  endpoint:", "  listen_port: -1\n  endpoint:", "listen_port"},
		{"large listen port", "  endpoint:", "  listen_port: 65536\n  endpoint:", "listen_port"},
		{"long hostname", "gateway.internal:51820", strings.Repeat("a", 254) + ":51820", "hostname"},
		{"long label", "gateway.internal:51820", strings.Repeat("a", 64) + ".internal:51820", "hostname"},
		{"empty label", "gateway.internal:51820", "gateway..internal:51820", "hostname"},
		{"label edge hyphen", "gateway.internal:51820", "gateway-.internal:51820", "hostname"},
		{"IPv6 zone", "gateway.internal:51820", "'[fe80::1%eth0]:51820'", "unicast host"},
		{"IPv6 worker", "10.88.0.2", "fd88::2", "usable host"},
		{"malformed key", publicKey(2), strings.Repeat("!", 44), "public_key"},
		{"long key", publicKey(2), base64.StdEncoding.EncodeToString(make([]byte, 33)), "public_key"},
		{"noncanonical key padding bits", publicKey(2), publicKey(2)[:42] + "J=", "public_key"},
		{"embedded key newline", publicKey(2), `"` + publicKey(2)[:20] + `\n` + publicKey(2)[20:] + `"`, "public_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(strings.Replace(validYAML(), tc.old, tc.replacement, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
		})
	}
	for _, count := range []int{0, 4097} {
		manifest, err := Load(strings.NewReader(validYAML()))
		if err != nil {
			t.Fatal(err)
		}
		manifest.Nodes = make([]Node, count)
		if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "between 1 and 4096") {
			t.Fatalf("%d workers: %v", count, err)
		}
		if files, err := manifest.Render(); err == nil || files != nil {
			t.Fatalf("invalid inventory rendered templates: %v, %v", files, err)
		}
	}
}

func TestSmallPrivateOverlayAndEndpointBoundaries(t *testing.T) {
	for _, subnet := range []string{"10.88.0.0/30", "172.16.0.0/30", "192.168.0.0/30"} {
		for _, endpoint := range []string{"127.0.0.1:1", "[::1]:65535", "Gateway.INTERNAL.:51820"} {
			manifest, err := Load(strings.NewReader(validYAML()))
			if err != nil {
				t.Fatal(err)
			}
			manifest.Network.CIDR = subnet
			prefix := strings.TrimSuffix(subnet, "0/30")
			manifest.Gateway.Address, manifest.Nodes[0].Address = prefix+"1", prefix+"2"
			manifest.Nodes = manifest.Nodes[:1]
			manifest.Gateway.Endpoint = endpoint
			if _, err := manifest.Render(); err != nil {
				t.Errorf("valid minimal overlay %s endpoint %s: %v", subnet, endpoint, err)
			}
		}
	}
}

func validYAML() string {
	return `version: 1
network:
  cidr: 10.88.0.0/24
gateway:
  name: gateway
  address: 10.88.0.1
  public_key: ` + publicKey(1) + `
  endpoint: gateway.internal:51820
nodes:
  - name: worker-a
    address: 10.88.0.2
    public_key: ` + publicKey(2) + `
  - name: worker-b
    address: 10.88.0.3
    public_key: ` + publicKey(3) + "\n"
}

func TestRejectUnsafeInventory(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"unknown version", "version: 1", "version: 2"},
		{"public overlay", "10.88.0.0/24", "8.8.8.0/24"},
		{"exit route", "10.88.0.0/24", "0.0.0.0/0"},
		{"noncanonical overlay", "10.88.0.0/24", "10.88.0.1/24"},
		{"IPv6 overlay", "10.88.0.0/24", "fd88::/64"},
		{"too small", "10.88.0.0/24", "10.88.0.0/31"},
		{"network address", "10.88.0.2", "10.88.0.0"},
		{"broadcast address", "10.88.0.2", "10.88.0.255"},
		{"outside overlay", "10.88.0.2", "10.89.0.2"},
		{"duplicate address", "10.88.0.2", "10.88.0.1"},
		{"duplicate name", "name: worker-a", "name: gateway"},
		{"duplicate key", publicKey(2), publicKey(1)},
		{"zero key", publicKey(2), publicKey(0)},
		{"short key", publicKey(2), "abcd"},
		{"unsafe filename", "name: worker-a", "name: ../worker"},
		{"long interface", "name: worker-a", "name: worker-123456789012345"},
		{"endpoint URL", "gateway.internal:51820", "https://gateway.internal:51820"},
		{"no endpoint port", "gateway.internal:51820", "gateway.internal"},
		{"invalid endpoint port", "gateway.internal:51820", "gateway.internal:65536"},
		{"wildcard endpoint", "gateway.internal:51820", "0.0.0.0:51820"},
		{"multicast endpoint", "gateway.internal:51820", "224.0.0.1:51820"},
		{"broadcast endpoint", "gateway.internal:51820", "255.255.255.255:51820"},
		{"recursive endpoint route", "gateway.internal:51820", "10.88.0.1:51820"},
		{"unknown field", "  cidr:", "  exit_route: true\n  cidr:"},
		{"private key collection", "  endpoint:", "  private_key: secret\n  endpoint:"},
		{"second document", "version: 1", "version: 1\n---\nversion: 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(strings.NewReader(strings.Replace(validYAML(), tc.old, tc.replacement, 1))); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
	if _, err := Load(strings.NewReader(strings.Repeat("#", maxManifestSize+1))); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestRenderGatewayRoutesAndLocalKeyTemplates(t *testing.T) {
	manifest, err := Load(strings.NewReader(validYAML()))
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.Render()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("files = %d, want 4", len(files))
	}
	gateway := string(files["gateway.conf"])
	for _, expected := range []string{"Address = 10.88.0.1/32", "ListenPort = 51820", "AllowedIPs = 10.88.0.2/32", "AllowedIPs = 10.88.0.3/32"} {
		if !strings.Contains(gateway, expected) {
			t.Errorf("gateway missing %q", expected)
		}
	}
	if strings.Contains(gateway, "AllowedIPs = 10.88.0.0/24") {
		t.Fatal("gateway peers must own individual /32 routes")
	}
	for _, name := range []string{"worker-a.conf", "worker-b.conf"} {
		config := string(files[name])
		for _, expected := range []string{"PublicKey = " + manifest.Gateway.PublicKey, "Endpoint = gateway.internal:51820", "AllowedIPs = 10.88.0.0/24", "PersistentKeepalive = 25"} {
			if !strings.Contains(config, expected) {
				t.Errorf("%s missing %q", name, expected)
			}
		}
	}
	for name, content := range files {
		if !strings.HasSuffix(name, ".conf") {
			continue
		}
		if !strings.Contains(string(content), "PrivateKey = "+privateKeyPlaceholder) || strings.Contains(string(content), "0.0.0.0/0") || strings.Contains(string(content), "PostUp") {
			t.Errorf("%s contains unexpected active secrets, exit routing, or host hooks", name)
		}
	}
	if !strings.Contains(string(files["README.md"]), "decrypts and re-encrypts") || !strings.Contains(string(files["README.md"]), "does not provide workspace isolation") {
		t.Fatal("deployment instructions omitted gateway trust and isolation boundaries")
	}
}

func TestListenPortAndIPv6Underlay(t *testing.T) {
	manifest, err := Load(strings.NewReader(strings.Replace(validYAML(), "endpoint: gateway.internal:51820", "endpoint: '[fd00::1]:51821'\n  listen_port: 51820", 1)))
	if err != nil {
		t.Fatal(err)
	}
	files, err := manifest.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files["gateway.conf"]), "ListenPort = 51820") || !strings.Contains(string(files["worker-a.conf"]), "Endpoint = [fd00::1]:51821") {
		t.Fatal("underlay endpoint and local listen port were conflated")
	}
}
