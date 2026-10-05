// Package admin is an organization's enterprise policy: who may sign in and how, how long
// credentials live, how identity-provider groups map to roles, which advanced areas of the
// dashboard are on, and how the organization is branded (DESIGN.md §25.8).
//
// It is policy only — values, defaults, validation, and the rules that apply them. Storage
// is internal/db, enforcement is internal/api, and the server's config file
// (internal/config.ServerFile) can lock any setting here for every organization on the
// server: a locked setting is applied over the stored one when the policy is read, and the
// API refuses to change it.
package admin

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/adamburan/conductor/internal/domain"
)

// GroupRule maps an identity-provider group to a role in one project.
type GroupRule struct {
	// Group is the group's name as the provider sends it: an OIDC groups claim value, a
	// SCIM group's displayName, or a GitHub team as "org/team-slug". Compared without case.
	Group string `json:"group" yaml:"group"`
	// Project is the project's slug; empty means the policy's default project.
	Project string      `json:"project,omitempty" yaml:"project"`
	Role    domain.Role `json:"role" yaml:"role"`
}

// Branding is how the organization presents itself in the dashboard. It is public: the
// sign-in page shows it before anyone has signed in.
type Branding struct {
	DisplayName string `json:"display_name,omitempty"`
	// AccentColor is "#rrggbb", checked for contrast against the light surface.
	AccentColor string `json:"accent_color,omitempty"`
	// LoginBanner is plain text shown on the sign-in page ("Authorized use only…"). It is
	// never interpreted as markup.
	LoginBanner string `json:"login_banner,omitempty"`
}

// Policy is one organization's settings. The zero value of every field means "the
// default", so a stored document only carries what an administrator changed.
type Policy struct {
	// RequireSSO, when on, accepts a human's token only if single sign-on minted it (or
	// local sign-in, which follows the security mode). Service principals are unaffected.
	RequireSSO bool `json:"require_sso"`
	// AllowedDomains and AllowedGitHubOrgs restrict who single sign-on admits into this
	// organization, on top of each provider's own restrictions.
	AllowedDomains    []string `json:"allowed_domains"`
	AllowedGitHubOrgs []string `json:"allowed_github_orgs"`
	// AutoProvision creates an account, in DefaultProject with DefaultRole, for a first
	// sign-in from an allowed domain or GitHub organization that matches no registered
	// address. DefaultRole is contributor, reviewer or observer.
	AutoProvision  bool        `json:"auto_provision"`
	DefaultRole    domain.Role `json:"default_role,omitempty"`
	DefaultProject string      `json:"default_project,omitempty"`
	// GroupRules map provider groups to project roles at every sign-in. No mapped role
	// ever exceeds MaxGroupRole (default maintainer; never org_admin).
	GroupRules   []GroupRule `json:"group_rules"`
	MaxGroupRole domain.Role `json:"max_group_role,omitempty"`
	// HumanTokenMaxTTL caps every token a person holds (default and ceiling: 90 days).
	// ServiceTokenMaxTTL caps tokens of service principals such as runners (zero: no cap).
	// SSOSessionTTL is the life of a token single sign-on mints (zero: the server's
	// --sso-token-ttl).
	HumanTokenMaxTTL   Duration `json:"human_token_max_ttl,omitempty"`
	ServiceTokenMaxTTL Duration `json:"service_token_max_ttl,omitempty"`
	SSOSessionTTL      Duration `json:"sso_session_ttl,omitempty"`
	// Features turns advanced dashboard areas on (see Features). Absent means off.
	Features map[string]bool `json:"features"`
	Branding Branding        `json:"branding"`
}

// MaxHumanTokenTTL is the ceiling on every human credential, whatever a policy says.
const MaxHumanTokenTTL = 90 * 24 * time.Hour

// minTokenTTL keeps a policy from making credentials useless.
const minTokenTTL = time.Hour

// Defaults is the policy of an organization nobody has configured.
func Defaults() Policy {
	return Policy{DefaultRole: domain.RoleContributor, MaxGroupRole: domain.RoleMaintainer,
		AllowedDomains: []string{}, AllowedGitHubOrgs: []string{}, GroupRules: []GroupRule{},
		Features: map[string]bool{}}
}

// Normalize fills defaults and canonicalizes lists, so two equivalent policies compare and
// store identically.
func (p *Policy) Normalize() {
	if p.DefaultRole == "" {
		p.DefaultRole = domain.RoleContributor
	}
	if p.MaxGroupRole == "" {
		p.MaxGroupRole = domain.RoleMaintainer
	}
	p.AllowedDomains = normalizeList(p.AllowedDomains, func(s string) string {
		return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
	})
	p.AllowedGitHubOrgs = normalizeList(p.AllowedGitHubOrgs, func(s string) string { return strings.ToLower(strings.TrimSpace(s)) })
	if p.GroupRules == nil {
		p.GroupRules = []GroupRule{}
	}
	for i := range p.GroupRules {
		p.GroupRules[i].Group = strings.TrimSpace(p.GroupRules[i].Group)
		p.GroupRules[i].Project = strings.TrimSpace(p.GroupRules[i].Project)
	}
	if p.Features == nil {
		p.Features = map[string]bool{}
	}
	p.Branding.DisplayName = strings.TrimSpace(p.Branding.DisplayName)
	p.Branding.AccentColor = strings.ToLower(strings.TrimSpace(p.Branding.AccentColor))
	if c, ok := expandHex(p.Branding.AccentColor); ok {
		p.Branding.AccentColor = c
	}
	p.Branding.LoginBanner = strings.TrimSpace(strings.ReplaceAll(p.Branding.LoginBanner, "\r\n", "\n"))
}

func normalizeList(in []string, f func(string) string) []string {
	out := []string{}
	for _, s := range in {
		if s = f(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ValidationError lists every problem with a policy, each naming its setting.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// Is makes a ValidationError an invalid argument wherever errors are mapped to statuses.
func (e *ValidationError) Is(target error) bool { return target == domain.ErrInvalidArgument }

var (
	domainPattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	githubOrgPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38})$`)
	slugPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// provisionRoles are the roles a self-service account may start with: the same bound the
// server's --sso-auto-provision has. Anything that can administer, maintain or run a
// project is granted by a person.
var provisionRoles = []domain.Role{domain.RoleContributor, domain.RoleReviewer, domain.RoleObserver}

// mappableRoles are the roles group mapping may grant. org_admin is never among them: an
// organization's administrators are named by a person, not by a group in another system.
var mappableRoles = []domain.Role{domain.RoleObserver, domain.RoleReviewer, domain.RoleContributor,
	domain.RoleMaintainer, domain.RoleProjectAdmin}

// Validate checks a normalized policy.
func (p Policy) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	for _, d := range p.AllowedDomains {
		if !domainPattern.MatchString(d) {
			add("allowed_domains: %q is not a domain name", d)
		}
	}
	for _, o := range p.AllowedGitHubOrgs {
		if !githubOrgPattern.MatchString(o) {
			add("allowed_github_orgs: %q is not a GitHub organization name", o)
		}
	}
	if !slices.Contains(provisionRoles, p.DefaultRole) {
		add("default_role must be contributor, reviewer or observer, not %q: anything more is granted by a person", p.DefaultRole)
	}
	if p.DefaultProject != "" && !slugPattern.MatchString(p.DefaultProject) {
		add("default_project: %q is not a project slug", p.DefaultProject)
	}
	if p.AutoProvision {
		if p.DefaultProject == "" {
			add("auto_provision needs default_project: the project new accounts join")
		}
		if len(p.AllowedDomains) == 0 && len(p.AllowedGitHubOrgs) == 0 {
			add("auto_provision would admit every account the identity provider has; restrict it with allowed_domains or allowed_github_orgs")
		}
	}
	if !slices.Contains(mappableRoles, p.MaxGroupRole) {
		add("max_group_role must be one of observer, reviewer, contributor, maintainer, project_admin, not %q", p.MaxGroupRole)
	}
	for i, r := range p.GroupRules {
		where := fmt.Sprintf("group_rules[%d]", i)
		switch {
		case r.Group == "" || len(r.Group) > 256:
			add("%s: group must be 1 to 256 characters", where)
		case !slices.Contains(mappableRoles, r.Role):
			add("%s: role must be one of observer, reviewer, contributor, maintainer, project_admin, not %q", where, r.Role)
		case !p.MaxGroupRole.Can(r.Role):
			add("%s: %s is above max_group_role (%s)", where, r.Role, p.MaxGroupRole)
		}
		if r.Project == "" && p.DefaultProject == "" {
			add("%s: name a project, or set default_project", where)
		} else if r.Project != "" && !slugPattern.MatchString(r.Project) {
			add("%s: %q is not a project slug", where, r.Project)
		}
	}
	if len(p.GroupRules) > 500 {
		add("group_rules: at most 500 rules")
	}
	if t := p.HumanTokenMaxTTL.Std(); t != 0 && (t < minTokenTTL || t > MaxHumanTokenTTL) {
		add("human_token_max_ttl must be between 1h and 90d, not %s", p.HumanTokenMaxTTL)
	}
	if t := p.ServiceTokenMaxTTL.Std(); t != 0 && t < minTokenTTL {
		add("service_token_max_ttl must be at least 1h (or 0 for no cap), not %s", p.ServiceTokenMaxTTL)
	}
	if t := p.SSOSessionTTL.Std(); t != 0 {
		if t < 5*time.Minute {
			add("sso_session_ttl must be at least 5m, not %s", p.SSOSessionTTL)
		} else if t > p.HumanTokenCap() {
			add("sso_session_ttl (%s) is longer than human_token_max_ttl (%s)", p.SSOSessionTTL, Duration(p.HumanTokenCap()))
		}
	}
	for name := range p.Features {
		if FeatureByKey(name) == nil {
			add("features: %q is not a feature (known: %s)", name, strings.Join(FeatureKeys(), ", "))
		}
	}
	if err := p.Branding.Validate(); err != nil {
		var v *ValidationError
		if errors.As(err, &v) {
			problems = append(problems, v.Problems...)
		}
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// HumanTokenCap is the longest a person's token may live under this policy.
func (p Policy) HumanTokenCap() time.Duration {
	if t := p.HumanTokenMaxTTL.Std(); t > 0 && t < MaxHumanTokenTTL {
		return t
	}
	return MaxHumanTokenTTL
}

// CapTokenTTL bounds a token lifetime for a principal of the given kind. ttl zero means "no
// expiry", which only a service principal may have, and only while its cap is unset.
func (p Policy) CapTokenTTL(kind domain.PrincipalKind, ttl time.Duration) time.Duration {
	limit := p.ServiceTokenMaxTTL.Std()
	if kind == domain.PrincipalHuman || kind == "" {
		limit = p.HumanTokenCap()
	}
	if limit > 0 && (ttl == 0 || ttl > limit) {
		return limit
	}
	return ttl
}

// EmailAllowed applies the organization's domain restriction to a verified address.
func (p Policy) EmailAllowed(email string) bool {
	if len(p.AllowedDomains) == 0 {
		return true
	}
	_, d, ok := strings.Cut(strings.ToLower(email), "@")
	return ok && slices.Contains(p.AllowedDomains, d)
}

// GitHubOrgAllowed reports whether any of a GitHub account's organizations is allowed.
func (p Policy) GitHubOrgAllowed(orgs []string) bool {
	if len(p.AllowedGitHubOrgs) == 0 {
		return true
	}
	for _, o := range orgs {
		if slices.Contains(p.AllowedGitHubOrgs, strings.ToLower(o)) {
			return true
		}
	}
	return false
}

// Restricted reports whether the policy narrows who single sign-on admits.
func (p Policy) Restricted() bool { return len(p.AllowedDomains) > 0 || len(p.AllowedGitHubOrgs) > 0 }

// FeatureOn reports whether an advanced area is enabled.
func (p Policy) FeatureOn(key string) bool { return p.Features[key] }

// printableText refuses control characters other than newlines and tabs: a banner or a name
// is shown as text, and an escape sequence or a bidirectional override in it is a way to
// make the page say something it does not.
func printableText(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
