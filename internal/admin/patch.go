package admin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aburan28/conductor/internal/domain"
)

// Patch is a partial policy: the settings an administrator changes in one request, or the
// settings a server's config file locks. A nil field leaves the setting alone.
type Patch struct {
	RequireSSO         *bool           `json:"require_sso,omitempty" yaml:"require_sso"`
	AllowedDomains     *[]string       `json:"allowed_domains,omitempty" yaml:"allowed_domains"`
	AllowedGitHubOrgs  *[]string       `json:"allowed_github_orgs,omitempty" yaml:"allowed_github_orgs"`
	AutoProvision      *bool           `json:"auto_provision,omitempty" yaml:"auto_provision"`
	DefaultRole        *domain.Role    `json:"default_role,omitempty" yaml:"default_role"`
	DefaultProject     *string         `json:"default_project,omitempty" yaml:"default_project"`
	GroupRules         *[]GroupRule    `json:"group_rules,omitempty" yaml:"group_rules"`
	MaxGroupRole       *domain.Role    `json:"max_group_role,omitempty" yaml:"max_group_role"`
	HumanTokenMaxTTL   *Duration       `json:"human_token_max_ttl,omitempty" yaml:"human_token_max_ttl"`
	ServiceTokenMaxTTL *Duration       `json:"service_token_max_ttl,omitempty" yaml:"service_token_max_ttl"`
	SSOSessionTTL      *Duration       `json:"sso_session_ttl,omitempty" yaml:"sso_session_ttl"`
	Features           map[string]bool `json:"features,omitempty" yaml:"features"`
	Branding           *BrandingPatch  `json:"branding,omitempty" yaml:"branding"`
}

// BrandingPatch is a partial branding.
type BrandingPatch struct {
	DisplayName *string `json:"display_name,omitempty" yaml:"display_name"`
	AccentColor *string `json:"accent_color,omitempty" yaml:"accent_color"`
	LoginBanner *string `json:"login_banner,omitempty" yaml:"login_banner"`
}

// Setting keys, as the API reports locks and the audit log records changes.
const (
	KeyRequireSSO         = "require_sso"
	KeyAllowedDomains     = "allowed_domains"
	KeyAllowedGitHubOrgs  = "allowed_github_orgs"
	KeyAutoProvision      = "auto_provision"
	KeyDefaultRole        = "default_role"
	KeyDefaultProject     = "default_project"
	KeyGroupRules         = "group_rules"
	KeyMaxGroupRole       = "max_group_role"
	KeyHumanTokenMaxTTL   = "human_token_max_ttl"
	KeyServiceTokenMaxTTL = "service_token_max_ttl"
	KeySSOSessionTTL      = "sso_session_ttl"
	KeyDisplayName        = "branding.display_name"
	KeyAccentColor        = "branding.accent_color"
	KeyLoginBanner        = "branding.login_banner"
	KeyLogo               = "branding.logo"
	// A feature's key is "features.<name>".
	featurePrefix = "features."
)

// FeatureKey is the setting key of a feature flag.
func FeatureKey(name string) string { return featurePrefix + name }

// Keys lists the settings the patch sets, sorted.
func (p Patch) Keys() []string {
	var out []string
	add := func(set bool, key string) {
		if set {
			out = append(out, key)
		}
	}
	add(p.RequireSSO != nil, KeyRequireSSO)
	add(p.AllowedDomains != nil, KeyAllowedDomains)
	add(p.AllowedGitHubOrgs != nil, KeyAllowedGitHubOrgs)
	add(p.AutoProvision != nil, KeyAutoProvision)
	add(p.DefaultRole != nil, KeyDefaultRole)
	add(p.DefaultProject != nil, KeyDefaultProject)
	add(p.GroupRules != nil, KeyGroupRules)
	add(p.MaxGroupRole != nil, KeyMaxGroupRole)
	add(p.HumanTokenMaxTTL != nil, KeyHumanTokenMaxTTL)
	add(p.ServiceTokenMaxTTL != nil, KeyServiceTokenMaxTTL)
	add(p.SSOSessionTTL != nil, KeySSOSessionTTL)
	for name := range p.Features {
		out = append(out, FeatureKey(name))
	}
	if b := p.Branding; b != nil {
		add(b.DisplayName != nil, KeyDisplayName)
		add(b.AccentColor != nil, KeyAccentColor)
		add(b.LoginBanner != nil, KeyLoginBanner)
	}
	sort.Strings(out)
	return out
}

// Empty reports whether the patch sets nothing.
func (p Patch) Empty() bool { return len(p.Keys()) == 0 }

// Apply returns base with the patch's settings applied and normalized. base is not changed.
func (p Patch) Apply(base Policy) Policy {
	out := base
	out.AllowedDomains = append([]string(nil), base.AllowedDomains...)
	out.AllowedGitHubOrgs = append([]string(nil), base.AllowedGitHubOrgs...)
	out.GroupRules = append([]GroupRule(nil), base.GroupRules...)
	out.Features = map[string]bool{}
	for k, v := range base.Features {
		out.Features[k] = v
	}
	if p.RequireSSO != nil {
		out.RequireSSO = *p.RequireSSO
	}
	if p.AllowedDomains != nil {
		out.AllowedDomains = append([]string(nil), (*p.AllowedDomains)...)
	}
	if p.AllowedGitHubOrgs != nil {
		out.AllowedGitHubOrgs = append([]string(nil), (*p.AllowedGitHubOrgs)...)
	}
	if p.AutoProvision != nil {
		out.AutoProvision = *p.AutoProvision
	}
	if p.DefaultRole != nil {
		out.DefaultRole = *p.DefaultRole
	}
	if p.DefaultProject != nil {
		out.DefaultProject = *p.DefaultProject
	}
	if p.GroupRules != nil {
		out.GroupRules = append([]GroupRule(nil), (*p.GroupRules)...)
	}
	if p.MaxGroupRole != nil {
		out.MaxGroupRole = *p.MaxGroupRole
	}
	if p.HumanTokenMaxTTL != nil {
		out.HumanTokenMaxTTL = *p.HumanTokenMaxTTL
	}
	if p.ServiceTokenMaxTTL != nil {
		out.ServiceTokenMaxTTL = *p.ServiceTokenMaxTTL
	}
	if p.SSOSessionTTL != nil {
		out.SSOSessionTTL = *p.SSOSessionTTL
	}
	for k, v := range p.Features {
		out.Features[k] = v
	}
	if b := p.Branding; b != nil {
		if b.DisplayName != nil {
			out.Branding.DisplayName = *b.DisplayName
		}
		if b.AccentColor != nil {
			out.Branding.AccentColor = *b.AccentColor
		}
		if b.LoginBanner != nil {
			out.Branding.LoginBanner = *b.LoginBanner
		}
	}
	out.Normalize()
	return out
}

// Locks are the settings a server's config file decides for every organization.
type Locks struct {
	// Patch holds the locked values.
	Patch Patch
	// Logo, when set, is the locked logo.
	Logo *Logo
}

// Keys lists the locked settings, sorted.
func (l Locks) Keys() []string {
	keys := l.Patch.Keys()
	if l.Logo != nil {
		keys = append(keys, KeyLogo)
		sort.Strings(keys)
	}
	return keys
}

// Locked reports whether a setting is locked.
func (l Locks) Locked(key string) bool {
	for _, k := range l.Keys() {
		if k == key {
			return true
		}
	}
	return false
}

// Effective is the policy in force: the stored policy with the locked settings applied
// over it.
func (l Locks) Effective(stored Policy) Policy { return l.Patch.Apply(stored) }

// ErrLocked names settings a request tried to change that the config file decides.
type ErrLocked struct{ Keys []string }

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("%s managed by the server's config file and cannot be changed here",
		strings.Join(e.Keys, ", ")+map[bool]string{true: " is", false: " are"}[len(e.Keys) == 1])
}

// Is makes a locked setting a permission refusal wherever errors are mapped to statuses.
func (e *ErrLocked) Is(target error) bool { return target == domain.ErrNotPermitted }

// CheckUnlocked refuses a patch that touches a locked setting.
func (l Locks) CheckUnlocked(p Patch) error {
	var hit []string
	for _, k := range p.Keys() {
		if l.Locked(k) {
			hit = append(hit, k)
		}
	}
	if len(hit) > 0 {
		return &ErrLocked{Keys: hit}
	}
	return nil
}
