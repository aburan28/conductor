package nat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
)

// Both IGD generations use WANIPConnection:1 and WANPPPConnection:1;
// IGD2 additionally supports WANIPConnection:2.
type portMapper interface {
	GetExternalIPAddressCtx(context.Context) (string, error)
	GetSpecificPortMappingEntryCtx(context.Context, string, uint16, string) (uint16, string, bool, string, uint32, error)
	AddPortMappingCtx(context.Context, string, uint16, string, uint16, string, bool, string, uint32) error
	DeletePortMappingCtx(context.Context, string, uint16, string) error
}

type gateway struct {
	mapper    portMapper
	location  *url.URL
	localAddr net.IP
}

func discoverGateways(ctx context.Context) ([]gateway, error) {
	var roots [2][]goupnp.MaybeRootDevice
	var failures [2]error
	var wg sync.WaitGroup
	for i, target := range []string{
		"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
		"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	} {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			roots[i], failures[i] = goupnp.DiscoverDevicesCtx(ctx, target)
		}(i, target)
	}
	wg.Wait()
	var gateways []gateway
	for _, discovered := range roots {
		for _, root := range discovered {
			if root.Err != nil || root.Root == nil {
				continue
			}
			ip2, _ := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(root.Root, root.Location)
			for _, client := range ip2 {
				gateways = append(gateways, gateway{client, root.Location, root.LocalAddr})
			}
			ip1, _ := internetgateway2.NewWANIPConnection1ClientsFromRootDevice(root.Root, root.Location)
			for _, client := range ip1 {
				gateways = append(gateways, gateway{client, root.Location, root.LocalAddr})
			}
			ppp, _ := internetgateway2.NewWANPPPConnection1ClientsFromRootDevice(root.Root, root.Location)
			for _, client := range ppp {
				gateways = append(gateways, gateway{client, root.Location, root.LocalAddr})
			}
		}
	}
	if len(gateways) == 0 {
		return nil, fmt.Errorf("no UPnP IGD1/IGD2 WANIP/WANPPP gateway found: %w", errors.Join(failures[0], failures[1], errors.New("enable router UPnP or use tailscale mode")))
	}
	return gateways, nil
}

func chooseInternalIP(ctx context.Context, o Options, gw gateway) (string, error) {
	host, _, _ := net.SplitHostPort(o.Addr)
	chosen := net.ParseIP(o.InternalIP)
	if chosen == nil && host != "" && host != "0.0.0.0" {
		chosen = net.ParseIP(host)
	}
	if chosen == nil && privateIPv4(gw.localAddr) {
		chosen = gw.localAddr
	}
	if chosen == nil && gw.location != nil {
		// Connecting a UDP socket chooses the route's source address without
		// transmitting a packet; no external STUN service is needed.
		conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", net.JoinHostPort(gw.location.Hostname(), "9"))
		if err != nil {
			return "", fmt.Errorf("select LAN route to gateway: %w", err)
		}
		chosen = conn.LocalAddr().(*net.UDPAddr).IP
		_ = conn.Close()
	}
	if !privateIPv4(chosen) {
		return "", fmt.Errorf("could not select a private LAN IPv4 address; set --nat-internal-ip")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("read local interfaces: %w", err)
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.Equal(chosen) {
			return chosen.String(), nil
		}
	}
	return "", fmt.Errorf("NAT internal IP %s is not assigned to this machine", chosen)
}

func publicIPv4(raw string) bool {
	ip, err := netip.ParseAddr(raw)
	shared := netip.MustParsePrefix("100.64.0.0/10")
	return err == nil && ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !shared.Contains(ip)
}

func mappingAbsent(err error) bool {
	var fault *soap.SOAPFaultError
	return errors.As(err, &fault) && fault.Detail.UPnPError.Errorcode == 714 // NoSuchEntryInArray
}

func startUPnP(ctx context.Context, o Options, a adapters) (Status, func(context.Context) error, func(context.Context) error, error) {
	gateways, err := a.discover(ctx)
	if err != nil {
		return Status{}, nil, nil, err
	}
	var failures []error
	_, port, _ := net.SplitHostPort(o.Addr)
	internalPort, _ := strconv.Atoi(port)
	externalPort := o.ExternalPort
	if externalPort == 0 {
		externalPort = internalPort
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Status{}, nil, nil, err
	}
	description := "Conductor-" + hex.EncodeToString(nonce[:])
	for _, gw := range gateways {
		wan, err := gw.mapper.GetExternalIPAddressCtx(ctx)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !publicIPv4(wan) {
			failures = append(failures, fmt.Errorf("gateway WAN IP %q is not public IPv4; UPnP cannot traverse CGNAT or double NAT; use tailscale mode", wan))
			continue
		}
		internalIP, err := a.internalIP(ctx, o, gw)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		mapper := gw.mapper
		owned := func(check context.Context) (bool, error) {
			p, ip, enabled, desc, _, err := mapper.GetSpecificPortMappingEntryCtx(check, "", uint16(externalPort), "TCP")
			if mappingAbsent(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if p != uint16(internalPort) || ip != internalIP || desc != description || !enabled {
				return false, fmt.Errorf("UPnP TCP port %d is owned by another mapping; refusing to modify it", externalPort)
			}
			return true, nil
		}
		// Refuse every existing mapping, including those to the same backend.
		_, _, _, _, _, err = mapper.GetSpecificPortMappingEntryCtx(ctx, "", uint16(externalPort), "TCP")
		if !mappingAbsent(err) {
			if err == nil {
				err = fmt.Errorf("UPnP TCP port %d already has a mapping", externalPort)
			}
			failures = append(failures, err)
			continue
		}
		add := func(addCtx context.Context) error {
			return mapper.AddPortMappingCtx(addCtx, "", uint16(externalPort), "TCP", uint16(internalPort), internalIP, true, description, uint32(o.Lease/time.Second))
		}
		cleanup := func(cleanCtx context.Context) error {
			exists, err := owned(cleanCtx)
			if err != nil || !exists {
				return err
			}
			return mapper.DeletePortMappingCtx(cleanCtx, "", uint16(externalPort), "TCP")
		}
		err = add(ctx)
		if err == nil {
			var exists bool
			exists, err = owned(ctx)
			if err == nil && !exists {
				err = fmt.Errorf("router did not retain the requested UPnP mapping")
			}
		}
		if err != nil {
			// A timed-out request may have succeeded on the router.
			cleanCtx, cancel := context.WithTimeout(context.Background(), o.cleanupTimeout())
			cleanErr := cleanup(cleanCtx)
			cancel()
			return Status{}, nil, nil, errors.Join(err, cleanErr)
		}
		renew := func(renewCtx context.Context) error {
			if _, err := owned(renewCtx); err != nil {
				return err
			}
			if err := add(renewCtx); err != nil {
				return err
			}
			exists, err := owned(renewCtx)
			if err == nil && !exists {
				return fmt.Errorf("router did not retain renewed UPnP mapping")
			}
			return err
		}
		// Mapping confirmation does not establish Internet reachability or
		// certificate validity for this IP. DNS certificates need --public-url.
		return Status{Mode: ModeUPnP, Endpoint: "https://" + net.JoinHostPort(wan, strconv.Itoa(externalPort)), InternalIP: internalIP, ExternalPort: externalPort, Active: true}, renew, cleanup, nil
	}
	return Status{}, nil, nil, fmt.Errorf("no usable UPnP gateway: %w", errors.Join(failures...))
}
