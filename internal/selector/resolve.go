package selector

import (
	"strings"

	"github.com/adamburan/conductor/internal/domain"
)

// Resolve fills in the catalog's opinion of a session's declared model.
//
// A session declares what it is running — "claude, claude-opus-5, effort xhigh". What that is
// *worth* (tier, capability tags, context window) comes from the organization's model
// catalog, not from the session. Otherwise any session could assert `tier: T4` and win every
// selection that requires a frontier model, which turns a capability floor into a suggestion.
//
// An unmatched model is not an error. The session keeps its declared model and effort and can
// still take work that names no tier or capability floor; it simply cannot claim capabilities
// nobody in the catalog vouched for.
func Resolve(caps domain.SessionCapabilities, harness string, profiles []domain.ModelProfile) domain.SessionCapabilities {
	caps.Tier = ""
	caps.Capabilities = nil
	caps.ContextWindow = 0
	caps.ProfileID = ""
	caps.Resolved = false

	profile, ok := matchProfile(caps, harness, profiles)
	if !ok {
		return caps
	}

	caps.Tier = profile.Tier
	caps.Capabilities = append([]string(nil), profile.Capabilities...)
	caps.ContextWindow = profile.ContextWindow
	caps.ProfileID = profile.ID
	caps.Resolved = true
	if caps.Alias == "" {
		caps.Alias = profile.Alias
	}
	// A session that named a model but no effort inherits the profile's default, which is
	// what the router would have given a fresh attempt on the same model.
	if caps.Effort == "" {
		caps.Effort = profile.ReasoningEffort
	}
	return caps
}

// matchProfile finds the catalog entry a session's declaration refers to, preferring an exact
// harness+model match, then the same model on any harness, then the declared alias.
func matchProfile(caps domain.SessionCapabilities, harness string, profiles []domain.ModelProfile) (domain.ModelProfile, bool) {
	var byModel, byAlias *domain.ModelProfile
	for i := range profiles {
		p := profiles[i]
		if !p.Enabled {
			continue
		}
		if caps.Model != "" && strings.EqualFold(p.Model, caps.Model) {
			if strings.EqualFold(p.Harness, harness) {
				return p, true
			}
			if byModel == nil {
				byModel = &profiles[i]
			}
		}
		if caps.Alias != "" && p.Alias == caps.Alias && strings.EqualFold(p.Harness, harness) {
			if byAlias == nil {
				byAlias = &profiles[i]
			}
		}
	}
	switch {
	case byModel != nil:
		return *byModel, true
	case byAlias != nil:
		return *byAlias, true
	default:
		return domain.ModelProfile{}, false
	}
}
