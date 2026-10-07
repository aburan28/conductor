package config

import "testing"

func TestServerConnectivityFlags(t *testing.T) {
	f, err := ParseServerFile([]byte(`version: 1
database:
  mode: rds
nat:
  mode: upnp
  internal_ip: 192.168.1.20
  external_port: 8443
  lease: 20m
  timeout: 3s
  https_port: 9443
`), func(string) string { return "" }, "")
	if err != nil {
		t.Fatal(err)
	}
	values := f.FlagValues()
	for key, want := range map[string]string{
		"database": "rds", "nat-mode": "upnp", "nat-internal-ip": "192.168.1.20",
		"nat-external-port": "8443", "nat-lease": "20m0s", "nat-timeout": "3s", "nat-https-port": "9443",
	} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
}

func TestServerConnectivityUnknownModes(t *testing.T) {
	for _, body := range []string{
		"version: 1\ndatabase:\n  mode: rdss\n",
		"version: 1\nnat:\n  mode: upmp\n",
	} {
		if _, err := ParseServerFile([]byte(body), func(string) string { return "" }, ""); err == nil {
			t.Fatalf("misspelled mode accepted: %s", body)
		}
	}
}
