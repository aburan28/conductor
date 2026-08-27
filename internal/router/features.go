package router

import (
	"strings"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/resource"
)

// FeaturePolicy declares which resource patterns mark a task as sensitive. It comes from
// .conductor/policies.yaml, so a project decides what "security sensitive" means in its own
// tree rather than inheriting someone else's guess.
type FeaturePolicy struct {
	SecurityPaths     []string
	CryptographyPaths []string
	SchemaOrMigration []string
	InfraPaths        []string
}

// DefaultFeaturePolicy is a conservative starting set for a repository that has not declared
// its own.
func DefaultFeaturePolicy() FeaturePolicy {
	return FeaturePolicy{
		SecurityPaths:     []string{"dir:internal/auth", "path:**/auth*.go", "dir:internal/privacy"},
		CryptographyPaths: []string{"dir:internal/privacy", "path:**/crypto/**"},
		SchemaOrMigration: []string{"dir:migrations", "migration:*", "schema:*"},
		InfraPaths:        []string{"dir:.github/workflows", "path:docker-compose.yml", "path:Dockerfile"},
	}
}

// FeaturePolicyFrom converts the project's stored feature paths into a FeaturePolicy,
// falling back to the defaults for any list the project left empty.
func FeaturePolicyFrom(p domain.FeaturePathPolicy) FeaturePolicy {
	out := DefaultFeaturePolicy()
	if len(p.SecuritySensitivePaths) > 0 {
		out.SecurityPaths = p.SecuritySensitivePaths
	}
	if len(p.CryptographySensitivePaths) > 0 {
		out.CryptographyPaths = p.CryptographySensitivePaths
	}
	if len(p.SchemaOrMigrationPaths) > 0 {
		out.SchemaOrMigration = p.SchemaOrMigrationPaths
	}
	if len(p.InfraPaths) > 0 {
		out.InfraPaths = p.InfraPaths
	}
	return out
}

// DeriveFeatures computes the router's feature record from scopes and attempt history
// (DESIGN.md §13.3).
//
// Everything here is derived from declared territory and ledger state. None of it requires
// reading a prompt or asking a model — which is what keeps the hot path free of an expensive
// coordinator (§4.10).
func DeriveFeatures(scopes []string, changedPaths []string, policy FeaturePolicy, base domain.TaskFeatures) domain.TaskFeatures {
	f := base

	parsed := make([]resource.Resource, 0, len(scopes))
	for _, s := range scopes {
		if r, err := resource.Parse(s); err == nil {
			parsed = append(parsed, r)
		}
	}

	f.SecuritySensitive = f.SecuritySensitive || matchesAny(parsed, policy.SecurityPaths)
	f.CryptographySensitive = f.CryptographySensitive || matchesAny(parsed, policy.CryptographyPaths)
	f.SchemaOrMigration = f.SchemaOrMigration || matchesAny(parsed, policy.SchemaOrMigration)
	f.InfraOrDeployment = f.InfraOrDeployment || matchesAny(parsed, policy.InfraPaths)

	for _, r := range parsed {
		if r.Type == domain.ResourceAPI {
			f.PublicAPIChange = true
		}
		if r.Type == domain.ResourceMigration || r.Type == domain.ResourceSchema {
			f.SchemaOrMigration = true
		}
	}

	// Estimated file count: prefer what is actually changing over what was declared, since
	// declarations are guesses and diffs are facts.
	if n := len(changedPaths); n > 0 {
		f.EstimatedFiles = n
	} else if f.EstimatedFiles == 0 {
		f.EstimatedFiles = estimateFiles(parsed)
	}

	f.CrossModuleEdges = countModules(parsed, changedPaths) - 1
	if f.CrossModuleEdges < 0 {
		f.CrossModuleEdges = 0
	}
	f.Languages = detectLanguages(changedPaths, parsed)
	return f
}

// ScopeGrowth reports how far actual work has drifted past the declared scope, which drives
// the replan rule (DESIGN.md §13.5).
func ScopeGrowth(declared []string, changedPaths []string) float64 {
	if len(changedPaths) == 0 {
		return 0
	}
	parsed := make([]resource.Resource, 0, len(declared))
	for _, s := range declared {
		if r, err := resource.Parse(s); err == nil {
			parsed = append(parsed, r)
		}
	}
	if len(parsed) == 0 {
		// Nothing was declared, so everything touched is drift.
		return float64(len(changedPaths))
	}

	outside := 0
	for _, p := range changedPaths {
		pr, err := resource.New(domain.ResourcePath, p)
		if err != nil {
			continue
		}
		covered := false
		for _, d := range parsed {
			if resource.Overlaps(pr, d) {
				covered = true
				break
			}
		}
		if !covered {
			outside++
		}
	}
	if outside == 0 {
		return 1
	}
	return float64(len(changedPaths)) / float64(len(changedPaths)-outside+1)
}

func matchesAny(scopes []resource.Resource, patterns []string) bool {
	for _, pattern := range patterns {
		pr, err := resource.Parse(pattern)
		if err != nil {
			continue
		}
		for _, s := range scopes {
			if resource.Overlaps(s, pr) {
				return true
			}
		}
	}
	return false
}

// estimateFiles guesses a file count from declared scopes when no diff exists yet. A
// directory or glob is treated as broad, a path as one file.
func estimateFiles(scopes []resource.Resource) int {
	n := 0
	for _, s := range scopes {
		switch s.Type {
		case domain.ResourceDir, domain.ResourceRepo:
			n += 8
		case domain.ResourceSchema, domain.ResourceMigration:
			n += 3
		default:
			if strings.ContainsAny(s.Key, "*?[") {
				n += 5
			} else {
				n++
			}
		}
	}
	return n
}

// countModules counts distinct top-two-segment prefixes, a cheap proxy for how many parts of
// the tree a change spans.
func countModules(scopes []resource.Resource, changed []string) int {
	mods := map[string]bool{}
	add := func(p string) {
		segs := strings.Split(p, "/")
		if len(segs) >= 2 {
			mods[segs[0]+"/"+segs[1]] = true
		} else if len(segs) == 1 && segs[0] != "" {
			mods[segs[0]] = true
		}
	}
	for _, s := range scopes {
		if s.PathLike() {
			add(s.Key)
		}
	}
	for _, p := range changed {
		add(p)
	}
	return len(mods)
}

func detectLanguages(changed []string, scopes []resource.Resource) []string {
	byExt := map[string]string{
		".go": "go", ".ts": "typescript", ".tsx": "typescript", ".js": "javascript",
		".jsx": "javascript", ".py": "python", ".rs": "rust", ".java": "java",
		".rb": "ruby", ".sql": "sql", ".sh": "shell", ".md": "markdown",
		".yaml": "yaml", ".yml": "yaml", ".json": "json",
	}
	seen := map[string]bool{}
	var out []string
	consider := func(p string) {
		if i := strings.LastIndex(p, "."); i >= 0 {
			if lang, ok := byExt[p[i:]]; ok && !seen[lang] {
				seen[lang] = true
				out = append(out, lang)
			}
		}
	}
	for _, p := range changed {
		consider(p)
	}
	for _, s := range scopes {
		if s.PathLike() {
			consider(s.Key)
		}
	}
	return out
}
