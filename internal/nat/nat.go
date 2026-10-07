// Package nat manages opt-in connectivity for the authenticated Conductor API.
// Database listeners are never inputs to this package or forwarded by it.
package nat

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	ModeOff       = "off"
	ModeUPnP      = "upnp"
	ModeTailscale = "tailscale"
)

type Options struct {
	Mode             string
	Addr             string
	TLSEnabled       bool
	SecurityMode     string
	InternalIP       string
	ExternalPort     int
	Lease            time.Duration
	Timeout          time.Duration
	HTTPSPort        int
	TailscaleCommand string
}

func (o Options) defaults() Options {
	if o.Mode == "" {
		o.Mode = ModeOff
	}
	if o.Lease == 0 {
		o.Lease = 30 * time.Minute
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	if o.HTTPSPort == 0 {
		o.HTTPSPort = 443
	}
	if o.TailscaleCommand == "" {
		o.TailscaleCommand = "tailscale"
	}
	return o
}

// Cleanup stays within daemon shutdown grace even when discovery is configured
// with a longer timeout.
func (o Options) cleanupTimeout() time.Duration {
	if o.Timeout > 5*time.Second {
		return 5 * time.Second
	}
	return o.Timeout
}

// Validate rejects unsafe exposure before database or network initialization.
// Tailscale terminates HTTPS and proxies to a loopback-only HTTP listener.
func (o Options) Validate() error {
	o = o.defaults()
	if o.Mode == ModeOff {
		return nil
	}
	if o.Mode != ModeUPnP && o.Mode != ModeTailscale {
		return fmt.Errorf("nat mode must be off, upnp, or tailscale")
	}
	if o.SecurityMode != "enhanced" {
		return fmt.Errorf("NAT connectivity requires enhanced security mode")
	}
	host, port, err := net.SplitHostPort(o.Addr)
	if err != nil {
		return fmt.Errorf("NAT API listen address: %w", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("NAT requires a fixed API listen port between 1 and 65535")
	}
	if o.Timeout <= 0 || o.Timeout > time.Minute {
		return fmt.Errorf("NAT timeout must be greater than zero and at most one minute")
	}
	if o.Mode == ModeTailscale {
		// An explicit IPv4 loopback avoids hostname resolution and Tailscale's
		// IPv4-only HTTP proxy backend restrictions.
		if host != "127.0.0.1" {
			return fmt.Errorf("tailscale mode requires API listening on 127.0.0.1")
		}
		if o.TLSEnabled {
			return fmt.Errorf("tailscale mode requires a loopback HTTP API; Tailscale supplies HTTPS")
		}
		if o.HTTPSPort < 1 || o.HTTPSPort > 65535 {
			return fmt.Errorf("NAT HTTPS port must be between 1 and 65535")
		}
		return nil
	}
	if !o.TLSEnabled {
		return fmt.Errorf("UPnP requires TLS on the API listener")
	}
	if o.Lease < time.Minute || o.Lease > 24*time.Hour {
		return fmt.Errorf("UPnP lease must be between one minute and 24 hours")
	}
	if o.ExternalPort < 0 || o.ExternalPort > 65535 {
		return fmt.Errorf("NAT external port must be between 1 and 65535, or zero to use the API port")
	}
	if host != "" && host != "0.0.0.0" {
		if !privateIPv4(net.ParseIP(host)) {
			return fmt.Errorf("UPnP API must bind a private IPv4 LAN address or 0.0.0.0")
		}
	}
	if o.InternalIP != "" {
		if !privateIPv4(net.ParseIP(o.InternalIP)) {
			return fmt.Errorf("NAT internal IP must be a private IPv4 LAN address")
		}
		if host != "" && host != "0.0.0.0" && host != o.InternalIP {
			return fmt.Errorf("NAT internal IP must match the bound API address")
		}
	}
	return nil
}

type Status struct {
	Mode         string `json:"mode"`
	Endpoint     string `json:"endpoint,omitempty"`
	InternalIP   string `json:"internal_ip,omitempty"`
	ExternalPort int    `json:"external_port,omitempty"`
	Active       bool   `json:"active"`
	LastError    string `json:"last_error,omitempty"`
}

type Manager struct {
	mu       sync.Mutex
	status   Status
	cancel   context.CancelFunc
	done     chan struct{}
	closeErr error
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Close cancels renewals and waits for bounded cleanup of the owned mapping.
// It is safe to call multiple times or concurrently.
func (m *Manager) Close() error {
	if m.cancel != nil {
		m.cancel()
	}
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeErr
}

type adapters struct {
	discover   func(context.Context) ([]gateway, error)
	internalIP func(context.Context, Options, gateway) (string, error)
	command    func(context.Context, string, ...string) ([]byte, error)
	// Tests supply ticks without shortening the router's lease.
	newTicker func(time.Duration) (<-chan time.Time, func())
}

func Start(ctx context.Context, opts Options, logger *slog.Logger) (*Manager, error) {
	return start(ctx, opts, logger, adapters{
		discover:   discoverGateways,
		internalIP: chooseInternalIP,
		command:    runCommand,
		newTicker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
	})
}

func start(ctx context.Context, opts Options, logger *slog.Logger, a adapters) (*Manager, error) {
	opts = opts.defaults()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{status: Status{Mode: opts.Mode}, done: make(chan struct{})}
	if opts.Mode == ModeOff {
		close(m.done)
		return m, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	setup, cancelSetup := context.WithTimeout(ctx, opts.Timeout)
	defer cancelSetup()
	var cleanup func(context.Context) error
	var renew func(context.Context) error
	var err error
	if opts.Mode == ModeUPnP {
		m.status, renew, cleanup, err = startUPnP(setup, opts, a)
	} else {
		m.status, cleanup, err = startTailscale(setup, opts, a.command)
	}
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	go func() {
		defer close(m.done)
		var ticks <-chan time.Time
		if renew != nil {
			var stop func()
			ticks, stop = a.newTicker(opts.Lease / 2)
			defer stop()
		}
		for {
			select {
			case <-life.Done():
				// The lifetime context is already cancelled, so cleanup receives
				// its own bounded context.
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), opts.cleanupTimeout())
				cleanupErr := cleanup(cleanupCtx)
				cleanupCancel()
				m.mu.Lock()
				m.closeErr = cleanupErr
				m.status.Active = false
				if cleanupErr != nil {
					m.status.LastError = cleanupErr.Error()
				}
				m.mu.Unlock()
				if cleanupErr != nil {
					logger.Warn("NAT cleanup failed", "mode", opts.Mode, "error", cleanupErr)
				}
				return
			case <-ticks:
				renewCtx, renewCancel := context.WithTimeout(life, opts.Timeout)
				renewErr := renew(renewCtx)
				renewCancel()
				m.mu.Lock()
				m.status.Active = renewErr == nil
				m.status.LastError = ""
				if renewErr != nil {
					m.status.LastError = renewErr.Error()
				}
				m.mu.Unlock()
				if renewErr != nil {
					logger.Warn("NAT lease renewal failed", "error", renewErr)
				}
			}
		}
	}()
	return m, nil
}

func privateIPv4(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && ip.IsPrivate()
}
