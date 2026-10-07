package nat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type serveConfig struct {
	TCP map[string]struct {
		HTTPS        bool
		HTTP         bool
		TCPForward   string
		TerminateTLS string
	} `json:"TCP"`
	Web map[string]struct {
		Handlers map[string]struct {
			Proxy    string
			Path     string
			Text     string
			Redirect string
		} `json:"Handlers"`
	} `json:"Web"`
	AllowFunnel map[string]bool        `json:"AllowFunnel"`
	Foreground  map[string]serveConfig `json:"Foreground"`
}

func (s serveConfig) portUsed(port string) bool {
	if _, ok := s.TCP[port]; ok {
		return true
	}
	for hp := range s.Web {
		if strings.HasSuffix(hp, ":"+port) {
			return true
		}
	}
	for hp, allowed := range s.AllowFunnel {
		if allowed && strings.HasSuffix(hp, ":"+port) {
			return true
		}
	}
	for _, child := range s.Foreground {
		if child.portUsed(port) {
			return true
		}
	}
	return false
}

func (s serveConfig) owns(port, hostPort, backend string) error {
	tcp, tcpOK := s.TCP[port]
	web, webOK := s.Web[hostPort]
	root, rootOK := web.Handlers["/"]
	if !tcpOK || !tcp.HTTPS || tcp.HTTP || tcp.TCPForward != "" || tcp.TerminateTLS != "" || !webOK || !rootOK || len(web.Handlers) != 1 || strings.TrimSuffix(root.Proxy, "/") != backend || root.Path != "" || root.Text != "" || root.Redirect != "" || s.AllowFunnel[hostPort] {
		return fmt.Errorf("tailscale serve port %s is not the owned HTTPS API proxy; refusing to modify it", port)
	}
	for hp := range s.Web {
		if hp != hostPort && strings.HasSuffix(hp, ":"+port) {
			return fmt.Errorf("tailscale serve port %s has additional handlers", port)
		}
	}
	for hp, allowed := range s.AllowFunnel {
		if allowed && strings.HasSuffix(hp, ":"+port) {
			return fmt.Errorf("tailscale serve port %s permits public Funnel traffic", port)
		}
	}
	for _, fg := range s.Foreground {
		if fg.portUsed(port) {
			return fmt.Errorf("tailscale serve port %s has a foreground owner", port)
		}
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale %s failed (check local daemon login, permissions, and tailnet HTTPS): %w", args[0], err)
	}
	return output, nil
}

func startTailscale(ctx context.Context, o Options, command func(context.Context, string, ...string) ([]byte, error)) (Status, func(context.Context) error, error) {
	readServe := func(c context.Context) (serveConfig, error) {
		output, err := command(c, o.TailscaleCommand, "serve", "status", "--json")
		if err != nil {
			return serveConfig{}, err
		}
		var config serveConfig
		if err := json.Unmarshal(output, &config); err != nil {
			return config, fmt.Errorf("parse tailscale serve status: %w", err)
		}
		return config, nil
	}
	config, err := readServe(ctx)
	if err != nil {
		return Status{}, nil, err
	}
	port := strconv.Itoa(o.HTTPSPort)
	if config.portUsed(port) {
		return Status{}, nil, fmt.Errorf("tailscale serve port %s is already configured; select another --nat-https-port", port)
	}
	output, err := command(ctx, o.TailscaleCommand, "status", "--json")
	if err != nil {
		return Status{}, nil, err
	}
	var state struct {
		BackendState string
		Self         *struct{ DNSName string }
	}
	if err := json.Unmarshal(output, &state); err != nil {
		return Status{}, nil, fmt.Errorf("parse tailscale node status: %w", err)
	}
	if state.BackendState != "Running" || state.Self == nil {
		return Status{}, nil, fmt.Errorf("tailscale daemon must be logged in and Running")
	}
	dnsName := strings.TrimSuffix(state.Self.DNSName, ".")
	if dnsName == "" || strings.ContainsAny(dnsName, "/:@?#\\ \t\r\n") {
		return Status{}, nil, fmt.Errorf("tailscale node has no valid DNS name; enable MagicDNS and tailnet HTTPS")
	}
	backend := "http://" + o.Addr
	hostPort := net.JoinHostPort(dnsName, port)
	cleanup := func(cleanCtx context.Context) error {
		current, err := readServe(cleanCtx)
		if err != nil {
			return err
		}
		if !current.portUsed(port) {
			return nil
		}
		// Do not delete a replaced proxy, another mount, a raw TCP forward,
		// a foreground session, or a now-public Funnel configuration.
		if err := current.owns(port, hostPort, backend); err != nil {
			return err
		}
		_, err = command(cleanCtx, o.TailscaleCommand, "serve", "--yes", "--https="+port, "off")
		return err
	}
	_, err = command(ctx, o.TailscaleCommand, "serve", "--bg", "--yes", "--https="+port, backend)
	if err == nil {
		var current serveConfig
		current, err = readServe(ctx)
		if err == nil {
			err = current.owns(port, hostPort, backend)
		}
	}
	if err != nil {
		// A command timeout can occur after tailscaled applies the config.
		cleanCtx, cancel := context.WithTimeout(context.Background(), o.cleanupTimeout())
		cleanErr := cleanup(cleanCtx)
		cancel()
		return Status{}, nil, errors.Join(err, cleanErr)
	}
	endpoint := "https://" + hostPort
	if o.HTTPSPort == 443 {
		endpoint = "https://" + dnsName
	}
	return Status{Mode: ModeTailscale, Endpoint: endpoint, InternalIP: "127.0.0.1", ExternalPort: o.HTTPSPort, Active: true}, cleanup, nil
}
