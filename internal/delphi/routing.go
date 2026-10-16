package delphi

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Target is one place a request can be sent: a provider, a model, and optionally an
// effort. It is written "provider:model" or "provider:model@effort"; a bare word names a
// profile.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

func (t Target) String() string {
	s := t.Provider + ":" + t.Model
	if t.Effort != "" {
		s += "@" + t.Effort
	}
	return s
}

// Profile is a named tier: which model a kind of work runs on, how hard it thinks, and
// where it goes when that model's safeguards decline. "implementer" might be Claude Haiku
// 5.5 at max effort; "frontier" Claude Mythos 5.1 falling back to an OpenAI model.
type Profile struct {
	Provider  string   `json:"provider"`
	Model     string   `json:"model,omitempty"`
	Effort    string   `json:"effort,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Fallback  []string `json:"fallback,omitempty"`
}

// Profiles maps profile names to profiles.
type Profiles map[string]Profile

// Names lists the profiles in order.
func (p Profiles) Names() []string {
	out := make([]string, 0, len(p))
	for n := range p {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LoadProfiles reads a JSON object of profiles. A missing file is no profiles.
func LoadProfiles(path string) (Profiles, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Profiles{}, nil
	}
	if err != nil {
		return nil, err
	}
	var p Profiles
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ParseTargets resolves a fallback chain. Each entry is "provider:model[@effort]" or a
// profile name, which contributes its own target (not its own fallbacks: chains do not
// nest, so a cycle cannot form).
func ParseTargets(specs []string, profiles Profiles, reg Registry) ([]Target, error) {
	var out []Target
	for _, raw := range specs {
		spec := strings.TrimSpace(raw)
		if spec == "" {
			continue
		}
		var t Target
		if prov, rest, ok := strings.Cut(spec, ":"); ok {
			t.Provider = prov
			t.Model, t.Effort, _ = strings.Cut(rest, "@")
		} else {
			p, ok := profiles[spec]
			if !ok {
				return nil, fmt.Errorf("fallback %q is neither provider:model nor a known profile (%s)", spec, strings.Join(profiles.Names(), ", "))
			}
			t = Target{Provider: p.Provider, Model: p.Model, Effort: p.Effort}
		}
		if reg != nil {
			prov, ok := reg[t.Provider]
			if !ok {
				return nil, fmt.Errorf("fallback %q: provider %q is not one of %v", spec, t.Provider, reg.Names())
			}
			if t.Model == "" {
				t.Model = prov.DefaultModel()
			}
		}
		if t.Model == "" {
			return nil, fmt.Errorf("fallback %q names no model", spec)
		}
		out = append(out, t)
	}
	return out, nil
}

// SplitList splits a comma-separated list, dropping empty entries.
func SplitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// OriginHash is the hash of the request as first submitted. A fallback rewrites provider
// and model, but resubmitting the original input must still be recognised as the same
// request, so it is never billed twice.
func OriginHash(r Request) string {
	if r.Origin != "" {
		return r.Origin
	}
	return Hash(r)
}

// NextFallback returns the request to send after a refusal, or false when the chain is
// exhausted. Provider-specific params and effort are dropped when the provider changes:
// they are written for one API and would be rejected or misread by the other.
func NextFallback(r Request, reg Registry, reason string) (Request, bool) {
	next := r
	next.Origin = OriginHash(r)
	next.Attempt = 0
	next.FallbackFrom = append(append([]string(nil), r.FallbackFrom...), r.Provider+":"+r.Model+" "+reason)
	for len(next.Fallback) > 0 {
		t := next.Fallback[0]
		next.Fallback = next.Fallback[1:]
		prov, ok := reg[t.Provider]
		if !ok {
			next.FallbackFrom = append(next.FallbackFrom, t.String()+" skipped (provider not configured)")
			continue
		}
		try := next
		if t.Provider != r.Provider {
			try.Params, try.Effort = nil, ""
		}
		try.Provider, try.Model = t.Provider, t.Model
		if try.Model == "" {
			try.Model = prov.DefaultModel()
		}
		if t.Effort != "" {
			try.Effort = t.Effort
		}
		if try.Model == "" {
			next.FallbackFrom = append(next.FallbackFrom, t.String()+" skipped (no model)")
			continue
		}
		if bad := prov.Validate(try); len(bad) > 0 {
			next.FallbackFrom = append(next.FallbackFrom, t.String()+" skipped ("+strings.Join(bad, "; ")+")")
			continue
		}
		return try, true
	}
	return r, false
}
