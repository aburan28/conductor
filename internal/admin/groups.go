package admin

import (
	"sort"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
)

// MappedRoles returns, for each project slug a rule names, the highest role the groups map
// to there, never above MaxGroupRole. A project no matching rule names is absent.
func (p Policy) MappedRoles(groups []string) map[string]domain.Role {
	have := map[string]bool{}
	for _, g := range groups {
		have[strings.ToLower(strings.TrimSpace(g))] = true
	}
	out := map[string]domain.Role{}
	for _, r := range p.GroupRules {
		if !have[strings.ToLower(r.Group)] {
			continue
		}
		role := r.Role
		if !p.MaxGroupRole.Can(role) {
			// Validation refuses such a rule; a stored one that predates a lower maximum
			// is capped rather than honoured.
			role = p.MaxGroupRole
		}
		project := r.Project
		if project == "" {
			project = p.DefaultProject
		}
		if project == "" {
			continue
		}
		if cur, ok := out[project]; !ok || role.Can(cur) {
			out[project] = role
		}
	}
	return out
}

// RoleChange is what group mapping does to one membership.
type RoleChange string

const (
	// RoleKeep leaves the membership as it is.
	RoleKeep RoleChange = "keep"
	// RoleAdd creates a membership.
	RoleAdd RoleChange = "add"
	// RoleSet changes an existing membership's role, up or down.
	RoleSet RoleChange = "set"
)

// PlanRole decides what mapping does to a principal's membership in one project, given the
// role its groups map to there. Mapping owns the roles up to MaxGroupRole and nothing
// above: a membership a person granted above that ceiling (an org_admin, or a
// project_admin when the ceiling is maintainer) is never touched, and neither is a runner,
// which is a service capability rather than a rung on the ladder. The caller still refuses
// to demote a project's last administrator.
func (p Policy) PlanRole(current domain.Role, member bool, mapped domain.Role) RoleChange {
	switch {
	case !member:
		return RoleAdd
	case current == mapped, current == domain.RoleRunner:
		return RoleKeep
	case current.Can(p.MaxGroupRole) && current != p.MaxGroupRole:
		return RoleKeep
	}
	return RoleSet
}

// SortedProjects returns a mapping's project slugs in order, for deterministic application.
func SortedProjects(m map[string]domain.Role) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
