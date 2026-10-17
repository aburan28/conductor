package wireguard

import (
	"encoding/base64"
	"strings"
	"testing"
)

func publicKey(value byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
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
