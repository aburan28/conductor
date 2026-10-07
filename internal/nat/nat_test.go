package nat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huin/goupnp/soap"
)

func validUPnP() Options {
	return Options{Mode: ModeUPnP, Addr: "0.0.0.0:8443", TLSEnabled: true, SecurityMode: "enhanced", Lease: time.Minute, Timeout: time.Second}
}

func TestValidateExposure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      Options
		wantError bool
	}{
		{"off", Options{}, false},
		{"upnp", validUPnP(), false},
		{"tls required", Options{Mode: ModeUPnP, Addr: "0.0.0.0:8443", SecurityMode: "enhanced"}, true},
		{"local unsafe", Options{Mode: ModeUPnP, Addr: "0.0.0.0:8443", TLSEnabled: true, SecurityMode: "local"}, true},
		{"loopback unreachable", Options{Mode: ModeUPnP, Addr: "127.0.0.1:8443", TLSEnabled: true, SecurityMode: "enhanced"}, true},
		{"tailscale", Options{Mode: ModeTailscale, Addr: "127.0.0.1:8080", SecurityMode: "enhanced"}, false},
		{"tailscale wildcard unsafe", Options{Mode: ModeTailscale, Addr: "0.0.0.0:8080", SecurityMode: "enhanced"}, true},
		{"tailscale tls backend", Options{Mode: ModeTailscale, Addr: "127.0.0.1:8080", TLSEnabled: true, SecurityMode: "enhanced"}, true},
		{"fixed port", Options{Mode: ModeUPnP, Addr: "0.0.0.0:0", TLSEnabled: true, SecurityMode: "enhanced"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.Validate(); (err != nil) != tc.wantError {
				t.Fatalf("Validate() = %v; want error %v", err, tc.wantError)
			}
		})
	}
	if got := (Options{Timeout: time.Minute}).cleanupTimeout(); got != 5*time.Second {
		t.Fatalf("cleanup timeout = %v", got)
	}
}

func TestOffDoesNotDiscoverOrExecute(t *testing.T) {
	m, err := start(context.Background(), Options{}, nil, adapters{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status().Mode != ModeOff || m.Status().Active {
		t.Fatalf("off status = %+v", m.Status())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

type fakeMapper struct {
	mu                      sync.Mutex
	wan                     string
	exists                  bool
	internalPort            uint16
	internalIP, description string
	lease                   uint32
	adds, deletes           int
	protocol                string
	externalPort            uint16
	renewed                 chan struct{}
	changedChecked          chan struct{}
	blockDelete             bool
}

func absentMapping() error {
	e := &soap.SOAPFaultError{}
	e.Detail.UPnPError.Errorcode = 714
	return e
}

func (f *fakeMapper) GetExternalIPAddressCtx(context.Context) (string, error) { return f.wan, nil }
func (f *fakeMapper) GetSpecificPortMappingEntryCtx(_ context.Context, _ string, _ uint16, _ string) (uint16, string, bool, string, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return 0, "", false, "", 0, absentMapping()
	}
	if f.description == "another-application" && f.changedChecked != nil {
		select {
		case f.changedChecked <- struct{}{}:
		default:
		}
	}
	return f.internalPort, f.internalIP, true, f.description, f.lease, nil
}
func (f *fakeMapper) AddPortMappingCtx(ctx context.Context, remote string, external uint16, protocol string, internal uint16, ip string, enabled bool, desc string, lease uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if remote != "" || !enabled {
		return errors.New("invalid mapping")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists, f.externalPort, f.protocol = true, external, protocol
	f.internalPort, f.internalIP, f.description, f.lease = internal, ip, desc, lease
	f.adds++
	if f.adds > 1 && f.renewed != nil {
		select {
		case f.renewed <- struct{}{}:
		default:
		}
	}
	return nil
}
func (f *fakeMapper) DeletePortMappingCtx(ctx context.Context, _ string, external uint16, protocol string) error {
	f.mu.Lock()
	block := f.blockDelete
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if external != f.externalPort || protocol != "TCP" {
		return errors.New("wrong mapping deleted")
	}
	f.deletes++
	f.exists = false
	return nil
}

func fakeAdapters(f *fakeMapper, ticks <-chan time.Time) adapters {
	return adapters{
		discover:   func(context.Context) ([]gateway, error) { return []gateway{{mapper: f}}, nil },
		internalIP: func(context.Context, Options, gateway) (string, error) { return "192.168.1.10", nil },
		newTicker:  func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} },
	}
}

func TestUPnPRenewsAPIOnlyAndReleasesOnShutdown(t *testing.T) {
	f := &fakeMapper{wan: "203.0.113.8", renewed: make(chan struct{}, 1)}
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := validUPnP()
	o.ExternalPort = 9443
	m, err := start(ctx, o, nil, fakeAdapters(f, ticks))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Status(); !got.Active || got.Endpoint != "https://203.0.113.8:9443" {
		t.Fatalf("status = %+v", got)
	}
	ticks <- time.Now()
	select {
	case <-f.renewed:
	case <-time.After(time.Second):
		t.Fatal("lease not renewed")
	}
	cancel()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adds != 2 || f.deletes != 1 || f.internalPort != 8443 || f.externalPort != 9443 || f.protocol != "TCP" || f.lease != 60 {
		t.Fatalf("unexpected API mapping: adds=%d deletes=%d internal=%d external=%d protocol=%s lease=%d", f.adds, f.deletes, f.internalPort, f.externalPort, f.protocol, f.lease)
	}
	if f.exists || m.Status().Active {
		t.Fatal("mapping remained active after close")
	}
}

func TestUPnPRefusesCGNATAndExistingMappings(t *testing.T) {
	for _, tc := range []struct {
		name, wan string
		exists    bool
	}{
		{"CGNAT", "100.64.0.1", false}, {"double NAT", "192.168.1.1", false}, {"existing", "203.0.113.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeMapper{wan: tc.wan, exists: tc.exists}
			if _, err := start(context.Background(), validUPnP(), nil, fakeAdapters(f, nil)); err == nil {
				t.Fatal("unsafe gateway accepted")
			}
			if f.adds != 0 || f.deletes != 0 {
				t.Fatal("existing or unreachable mapping changed")
			}
		})
	}
}

func TestUPnPChangedOwnerIsNotRenewedOrDeleted(t *testing.T) {
	f := &fakeMapper{wan: "203.0.113.8", changedChecked: make(chan struct{}, 1)}
	ticks := make(chan time.Time, 1)
	m, err := start(context.Background(), validUPnP(), nil, fakeAdapters(f, ticks))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.description = "another-application"
	f.mu.Unlock()
	ticks <- time.Now()
	select {
	case <-f.changedChecked:
	case <-time.After(time.Second):
		t.Fatal("renewal did not check mapping ownership")
	}
	if err := m.Close(); err == nil {
		t.Fatal("changed owner cleanup should report refusal")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adds != 1 || f.deletes != 0 {
		t.Fatalf("changed mapping touched: adds=%d deletes=%d", f.adds, f.deletes)
	}
}

func TestUPnPCleanupDeadline(t *testing.T) {
	f := &fakeMapper{wan: "203.0.113.8", blockDelete: true}
	o := validUPnP()
	o.Timeout = 20 * time.Millisecond
	m, err := start(context.Background(), o, nil, fakeAdapters(f, nil))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := m.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cleanup exceeded bounded deadline")
	}
}

type fakeTailscale struct {
	config           string
	starts, stops    int
	changeAfterStart bool
}

func (f *fakeTailscale) command(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch strings.Join(args, " ") {
	case "serve status --json":
		return []byte(f.config), nil
	case "status --json":
		return []byte(`{"BackendState":"Running","Self":{"DNSName":"node.example.ts.net."}}`), nil
	case "serve --bg --yes --https=8443 http://127.0.0.1:8080":
		f.starts++
		proxy := "http://127.0.0.1:8080"
		if f.changeAfterStart {
			proxy = "http://127.0.0.1:9999"
		}
		f.config = `{"TCP":{"443":{"HTTPS":true},"8443":{"HTTPS":true}},"Web":{"unrelated.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7777"}}},"node.example.ts.net:8443":{"Handlers":{"/":{"Proxy":"` + proxy + `"}}}}}`
		return nil, nil
	case "serve --yes --https=8443 off":
		f.stops++
		f.config = `{"TCP":{"443":{"HTTPS":true}},"Web":{"unrelated.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7777"}}}}}`
		return nil, nil
	default:
		return nil, errors.New("unexpected command: " + strings.Join(args, " "))
	}
}

func tailscaleOptions() Options {
	return Options{Mode: ModeTailscale, Addr: "127.0.0.1:8080", SecurityMode: "enhanced", HTTPSPort: 8443}
}

func TestTailscaleOwnPortLifecyclePreservesOtherService(t *testing.T) {
	f := &fakeTailscale{config: `{ "TCP":{"443":{"HTTPS":true}} }`}
	m, err := start(context.Background(), tailscaleOptions(), nil, adapters{command: f.command})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Status(); !got.Active || got.Endpoint != "https://node.example.ts.net:8443" {
		t.Fatalf("status = %+v", got)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if f.starts != 1 || f.stops != 1 || !strings.Contains(f.config, "unrelated.ts.net:443") {
		t.Fatal("owned-port lifecycle did not preserve other service")
	}
}

func TestTailscalePortGuardAndConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		change       bool
		wantStarts   int
	}{
		{"existing HTTPS", `{"TCP":{"8443":{"HTTPS":true}}}`, false, 0},
		{"existing foreground", `{"Foreground":{"session":{"TCP":{"8443":{"HTTPS":true}}}}}`, false, 0},
		{"public Funnel", `{"AllowFunnel":{"node.example.ts.net:8443":true}}`, false, 0},
		{"malformed status", `not json`, false, 0},
		{"proxy not applied", `{}`, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTailscale{config: tc.config, changeAfterStart: tc.change}
			if _, err := start(context.Background(), tailscaleOptions(), nil, adapters{command: f.command}); err == nil {
				t.Fatal("unsafe Tailscale config accepted")
			}
			if f.starts != tc.wantStarts || f.stops != 0 {
				t.Fatalf("commands: starts=%d stops=%d", f.starts, f.stops)
			}
		})
	}
}

func TestTailscaleChangedOwnerIsNotDeleted(t *testing.T) {
	f := &fakeTailscale{config: `{}`}
	m, err := start(context.Background(), tailscaleOptions(), nil, adapters{command: f.command})
	if err != nil {
		t.Fatal(err)
	}
	f.config = strings.ReplaceAll(f.config, "http://127.0.0.1:8080", "http://127.0.0.1:9999")
	if err := m.Close(); err == nil {
		t.Fatal("changed proxy should refuse cleanup")
	}
	if f.stops != 0 {
		t.Fatal("another proxy was removed")
	}
}
