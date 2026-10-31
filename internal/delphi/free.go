package delphi

import (
	"fmt"
	"strings"
)

// FreeMode restricts delphi to targets that cost nothing ($DELPHI_FREE).
type FreeMode string

const (
	// FreeLocal allows only endpoints you run yourself (vLLM, LM Studio, a notebook you
	// tunnel to): free, and private, since prompts never reach a third party.
	FreeLocal FreeMode = "local"
	// FreePublic also allows third-party free tiers (OpenRouter `:free` models and the
	// like), whose providers may train on prompts: public or synthetic content only.
	FreePublic FreeMode = "public"
)

// ParseFreeMode reads $DELPHI_FREE: empty (off), "local" or "public".
func ParseFreeMode(s string) (FreeMode, error) {
	switch m := FreeMode(strings.TrimSpace(s)); m {
	case "", FreeLocal, FreePublic:
		return m, nil
	}
	return "", fmt.Errorf("DELPHI_FREE=%q: use local (your own endpoints only) or public (also third-party free tiers, which may train on prompts)", s)
}

// FreeKnower is implemented by providers that know some of their models cost nothing.
// local is true for an endpoint you run yourself. The native batch providers never are.
type FreeKnower interface {
	Free(model string) (free, local bool)
}

// FreePolicy is free mode: which targets are allowed, and what stands in for the rest.
type FreePolicy struct {
	Mode FreeMode
	// Substitutes maps a paid target to a free one. Keys are "provider:model",
	// "provider:*" or "*", tried in that order.
	Substitutes map[string]Target
}

// Allows reports whether t costs nothing under the policy, and why not when it does.
func (fp *FreePolicy) Allows(reg Registry, t Target) (bool, string) {
	fk, ok := reg[t.Provider].(FreeKnower)
	if !ok {
		return false, t.Provider + " is a paid API"
	}
	free, local := fk.Free(t.Model)
	switch {
	case !free:
		return false, t.String() + " is not free"
	case !local && fp.Mode != FreePublic:
		return false, t.String() + " is a third-party free tier, which DELPHI_FREE=local excludes (use public for public or synthetic content)"
	}
	return true, ""
}

func (fp *FreePolicy) substitute(t Target) (Target, bool) {
	for _, key := range []string{t.Provider + ":" + t.Model, t.Provider + ":*", "*"} {
		if s, ok := fp.Substitutes[key]; ok {
			return s, true
		}
	}
	return Target{}, false
}

// Apply rewrites r to a free target, or says why it cannot. A substitute that changes the
// provider drops effort and params, which are written for one provider's API, as a
// fallback does; the original target is kept in meta["free_substitute_for"].
func (fp *FreePolicy) Apply(r *Request, reg Registry) []string {
	var problems []string
	here := Target{Provider: r.Provider, Model: r.Model, Effort: r.Effort}
	if ok, why := fp.Allows(reg, here); !ok {
		sub, found := fp.substitute(here)
		if !found {
			return []string{fmt.Sprintf("free mode: %s; map it to a free model in the free-mode substitutes", why)}
		}
		if ok, why := fp.Allows(reg, sub); !ok {
			return []string{fmt.Sprintf("free mode: the substitute for %s is not allowed: %s", here, why)}
		}
		if sub.Provider != r.Provider {
			r.Params, r.Effort = nil, ""
		}
		r.Provider, r.Model = sub.Provider, sub.Model
		if sub.Effort != "" {
			r.Effort = sub.Effort
		}
		if r.Meta == nil {
			r.Meta = map[string]string{}
		}
		r.Meta["free_substitute_for"] = here.String()
	}
	// A fallback is another request: substitute it the same way, or drop it.
	var chain []Target
	for _, t := range r.Fallback {
		if ok, _ := fp.Allows(reg, t); ok {
			chain = append(chain, t)
		} else if sub, found := fp.substitute(t); found {
			if ok, _ := fp.Allows(reg, sub); ok {
				chain = append(chain, sub)
			}
		}
	}
	r.Fallback = chain
	return problems
}
