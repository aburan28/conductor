// Package modelsrc discovers which models an inference endpoint serves, so the model catalog
// can follow what providers release instead of what someone remembered to type into a file.
//
// Discovery reads model *names* from a provider's list endpoint and nothing else: it sends no
// prompt and receives no completion, the same posture as the rest of Conductor. The endpoint's
// key is opened from the project secret vault for the duration of one request.
package modelsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// Model is one entry of a provider's list.
type Model struct {
	ID          string
	DisplayName string
	// Created is when the provider says the model was released; zero when it does not say.
	Created       time.Time
	ContextWindow int
}

// Target is what a lister needs to make one request.
type Target struct {
	Kind    domain.InferenceKind
	BaseURL string
	Key     string
}

// Lister lists the models a target serves. The interface exists so the refresh loop can be
// tested without a network and so a provider with its own list shape can be added in one place.
type Lister interface {
	List(ctx context.Context, t Target) ([]Model, error)
}

// Limits on what one discovery request may cost, whatever the far end sends back.
const (
	requestTimeout = 20 * time.Second
	maxBodyBytes   = 8 << 20
	maxPages       = 20
	maxModels      = 5000
)

// HTTPLister is the production Lister.
type HTTPLister struct {
	client *http.Client
}

// NewHTTPLister returns a Lister that refuses redirects (a key must not follow a 30x to some
// other host) and refuses to connect to link-local or unspecified addresses, which is where
// cloud metadata services live. The check runs on the address actually dialled, so a hostname
// that resolves there is refused too.
//
// Behind an egress proxy from the environment (HTTPS_PROXY) the dialler only ever connects to
// the proxy, so the destination is checked before the proxy is asked for it, the same way the
// notification relay does.
func NewHTTPLister() *HTTPLister {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guardDial}
	return &HTTPLister{client: &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			Proxy: func(r *http.Request) (*url.URL, error) {
				proxy, err := http.ProxyFromEnvironment(r)
				if err != nil || proxy == nil {
					return proxy, err
				}
				if err := checkDestination(r.Context(), r.URL.Hostname()); err != nil {
					return nil, err
				}
				return proxy, nil
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// guardDial is net.Dialer.Control: it sees the resolved address just before connecting.
func guardDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	if blockedIP(ip) {
		return fmt.Errorf("refusing to connect to %s", ip)
	}
	return nil
}

// checkDestination refuses a host that is, or resolves to, an address discovery never
// connects to.
func checkDestination(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("refusing to connect to %s", ip)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil // the proxy resolves it; an unresolvable name fails there
	}
	for _, a := range addrs {
		if blockedIP(a.IP) {
			return fmt.Errorf("refusing to connect to %s: it resolves to %s", host, a.IP)
		}
	}
	return nil
}

// blockedIP reports addresses discovery never connects to, whatever the operator configured.
//
// A NAT64 (64:ff9b::/96) or 6to4 (2002::/16) address delivers to the IPv4 address inside it,
// so 64:ff9b::a9fe:a9fe is the metadata service and is judged as 169.254.169.254.
func blockedIP(ip net.IP) bool {
	if v6 := ip.To16(); v6 != nil && ip.To4() == nil {
		switch {
		case v6[0] == 0x00 && v6[1] == 0x64 && v6[2] == 0xff && v6[3] == 0x9b && isZero(v6[4:12]):
			return blockedIP(net.IPv4(v6[12], v6[13], v6[14], v6[15]))
		case v6[0] == 0x20 && v6[1] == 0x02:
			return blockedIP(net.IPv4(v6[2], v6[3], v6[4], v6[5]))
		}
	}
	return ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// privateOrLoopback reports addresses where cleartext http is acceptable: a model server on
// this machine or this network, not a key sent across the internet in the clear.
func privateOrLoopback(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate()
}

// ValidateBaseURL checks an endpoint address before it is stored. https is always fine; plain
// http only to localhost or a private address, for a local model server. A hostname that is
// not a literal cannot be judged here, so the dialer checks it again when it is resolved.
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: base_url must be an absolute http(s) address", domain.ErrInvalidArgument)
	}
	if u.User != nil {
		return fmt.Errorf("%w: base_url must not contain credentials; use api_key_secret", domain.ErrInvalidArgument)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: base_url must not carry a query or fragment", domain.ErrInvalidArgument)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip != nil && blockedIP(ip) {
		return fmt.Errorf("%w: base_url points at a link-local or unspecified address", domain.ErrInvalidArgument)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") ||
			(ip != nil && privateOrLoopback(ip)) {
			return nil
		}
		// A name on the local network ("gpu-box", "gpu-box.local") is the usual way a
		// home-lab model server is addressed, so single-label and .local names pass; the
		// dial check still refuses link-local results.
		if ip == nil && (!strings.Contains(host, ".") || strings.HasSuffix(strings.ToLower(host), ".local")) {
			return nil
		}
		return fmt.Errorf("%w: plain http is only allowed to localhost or a private address; use https", domain.ErrInvalidArgument)
	default:
		return fmt.Errorf("%w: base_url must be http or https", domain.ErrInvalidArgument)
	}
}

// List implements Lister.
func (l *HTTPLister) List(ctx context.Context, t Target) ([]Model, error) {
	if err := ValidateBaseURL(t.BaseURL); err != nil {
		return nil, err
	}
	switch t.Kind {
	case domain.InferenceAnthropic:
		return l.listAnthropic(ctx, t)
	case domain.InferenceOpenAI, domain.InferenceOpenAICompatible:
		return l.listOpenAI(ctx, t)
	case domain.InferenceOllama:
		return l.listOllama(ctx, t)
	default:
		return nil, fmt.Errorf("%w: unknown endpoint kind %q", domain.ErrInvalidArgument, t.Kind)
	}
}

// joinURL appends path to base without doubling a version segment the operator already
// included ("https://host/v1" + "/v1/models" is "https://host/v1/models").
func joinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

// get fetches one JSON document. Errors carry the status and a short provider-supplied type,
// never the response body, which could echo back anything the far end likes.
func (l *HTTPLister) get(ctx context.Context, target string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "conductor-model-discovery")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %s", scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading response: %s", scrub(err))
	}
	if len(body) > maxBodyBytes {
		return errors.New("response too large")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("HTTP %d: the endpoint rejected the API key", resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("HTTP %d: the endpoint redirected; set base_url to the final address", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d from the endpoint's model list", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("the endpoint's model list is not the expected JSON")
	}
	return nil
}

// scrub keeps a transport error short and free of the request URL.
func scrub(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (l *HTTPLister) listAnthropic(ctx context.Context, t Target) ([]Model, error) {
	var out []Model
	after := ""
	for page := 0; page < maxPages; page++ {
		q := "?limit=1000"
		if after != "" {
			q += "&after_id=" + url.QueryEscape(after)
		}
		var body struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				CreatedAt   string `json:"created_at"`
				MaxInput    int    `json:"max_input_tokens"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		err := l.get(ctx, joinURL(t.BaseURL, "/v1/models")+q, map[string]string{
			"x-api-key": t.Key, "anthropic-version": "2023-06-01",
		}, &body)
		if err != nil {
			return nil, err
		}
		for _, m := range body.Data {
			if m.ID == "" {
				continue
			}
			created, _ := time.Parse(time.RFC3339, m.CreatedAt)
			out = append(out, Model{ID: m.ID, DisplayName: m.DisplayName, Created: created, ContextWindow: m.MaxInput})
		}
		if !body.HasMore || body.LastID == "" || len(out) >= maxModels {
			break
		}
		after = body.LastID
	}
	return capModels(out), nil
}

func (l *HTTPLister) listOpenAI(ctx context.Context, t Target) ([]Model, error) {
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			// Context length is not part of OpenAI's shape, but several compatible servers
			// report it under one of these names.
			MaxModelLen   int `json:"max_model_len"`
			ContextLength int `json:"context_length"`
			ContextWindow int `json:"context_window"`
		} `json:"data"`
	}
	headers := map[string]string{}
	if t.Key != "" {
		headers["Authorization"] = "Bearer " + t.Key
	}
	if err := l.get(ctx, joinURL(t.BaseURL, "/v1/models"), headers, &body); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		model := Model{ID: m.ID}
		if m.Created > 0 {
			model.Created = time.Unix(m.Created, 0).UTC()
		}
		for _, n := range []int{m.MaxModelLen, m.ContextLength, m.ContextWindow} {
			if n > 0 {
				model.ContextWindow = n
				break
			}
		}
		out = append(out, model)
	}
	return capModels(out), nil
}

func (l *HTTPLister) listOllama(ctx context.Context, t Target) ([]Model, error) {
	var body struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model"`
			ModifiedAt string `json:"modified_at"`
		} `json:"models"`
	}
	if err := l.get(ctx, strings.TrimRight(t.BaseURL, "/")+"/api/tags", nil, &body); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(body.Models))
	for _, m := range body.Models {
		id := m.Name
		if id == "" {
			id = m.Model
		}
		if id == "" {
			continue
		}
		// modified_at is when the local copy was pulled, not a release date, but it still
		// orders a machine's models by recency, which is what "newest" means locally.
		modified, _ := time.Parse(time.RFC3339Nano, m.ModifiedAt)
		out = append(out, Model{ID: id, Created: modified})
	}
	return capModels(out), nil
}

func capModels(m []Model) []Model {
	if len(m) > maxModels {
		return m[:maxModels]
	}
	return m
}

// Preset is a well-known endpoint, so adding one is a name and a key rather than a URL.
type Preset struct {
	Name    string               `json:"name"`
	Kind    domain.InferenceKind `json:"kind"`
	BaseURL string               `json:"base_url"`
	// KeyHint is the usual name of the secret that holds this provider's key; empty when the
	// server is normally keyless.
	KeyHint string `json:"key_hint,omitempty"`
	Local   bool   `json:"local,omitempty"`
}

// Presets lists endpoints most teams want. None of them names a model.
var Presets = []Preset{
	{Name: "anthropic", Kind: domain.InferenceAnthropic, BaseURL: "https://api.anthropic.com", KeyHint: "ANTHROPIC_API_KEY"},
	{Name: "openai", Kind: domain.InferenceOpenAI, BaseURL: "https://api.openai.com/v1", KeyHint: "OPENAI_API_KEY"},
	{Name: "openrouter", Kind: domain.InferenceOpenAICompatible, BaseURL: "https://openrouter.ai/api/v1", KeyHint: "OPENROUTER_API_KEY"},
	{Name: "fireworks", Kind: domain.InferenceOpenAICompatible, BaseURL: "https://api.fireworks.ai/inference/v1", KeyHint: "FIREWORKS_API_KEY"},
	{Name: "ollama", Kind: domain.InferenceOllama, BaseURL: "http://localhost:11434", Local: true},
	{Name: "lmstudio", Kind: domain.InferenceOpenAICompatible, BaseURL: "http://localhost:1234/v1", Local: true},
	{Name: "vllm", Kind: domain.InferenceOpenAICompatible, BaseURL: "http://localhost:8000/v1", Local: true},
}

// PresetByName finds a preset.
func PresetByName(name string) (Preset, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}
