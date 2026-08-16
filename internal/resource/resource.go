// Package resource implements the reservation resource keys of DESIGN.md §11.1, the path
// normalization rules of §11.4, and the overlap algebra that the conflict matrix in §11.3
// is applied to.
//
// Everything here is pure. Given the same two resource strings it always produces the same
// answer, which is what makes conflict decisions reproducible and testable — a requirement
// of DESIGN.md §4.1 (deterministic state before probabilistic judgment).
package resource

import (
	"fmt"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/adamburan/conductor/internal/domain"
)

// Resource is a parsed, normalized reservation key.
type Resource struct {
	Type domain.ResourceType
	Key  string
}

func (r Resource) String() string { return string(r.Type) + ":" + r.Key }

// IsZero reports whether the resource is unset.
func (r Resource) IsZero() bool { return r.Type == "" && r.Key == "" }

// PathLike reports whether the resource addresses a location in the filesystem, and can
// therefore participate in prefix and glob comparisons.
func (r Resource) PathLike() bool {
	switch r.Type {
	case domain.ResourcePath, domain.ResourceDir, domain.ResourceLockfile, domain.ResourceTest:
		return true
	}
	return false
}

// Parse reads a canonical "type:key" resource string. It also accepts a bare path as a
// convenience for CLI callers: a trailing slash or a directory-looking value becomes a
// `dir:`, everything else a `path:`.
//
// The normalization here is load-bearing. Two principals must not be able to reserve the
// same file under two spellings ("./src/a.go" and "src/a.go") and both believe they hold it.
func Parse(s string) (Resource, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Resource{}, fmt.Errorf("%w: empty resource", domain.ErrInvalidArgument)
	}

	typ, key, found := strings.Cut(s, ":")
	if !found {
		// Bare path form.
		if strings.HasSuffix(s, "/") {
			return New(domain.ResourceDir, s)
		}
		return New(domain.ResourcePath, s)
	}

	rt := domain.ResourceType(typ)
	if err := domain.Validate(rt, domain.AllResourceTypes, "resource_type"); err != nil {
		// A colon that is not a known type prefix is far more likely to be a Windows drive
		// letter or an unprefixed path containing a colon than a typo'd type, so treat the
		// whole string as a path rather than silently reserving the wrong thing.
		return New(domain.ResourcePath, s)
	}
	return New(rt, key)
}

// MustParse is Parse for tests and constant configuration.
func MustParse(s string) Resource {
	r, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return r
}

// New builds a resource of an explicit type, applying that type's normalization.
func New(t domain.ResourceType, key string) (Resource, error) {
	key = strings.TrimSpace(key)
	if key == "" && t != domain.ResourceRepo {
		return Resource{}, fmt.Errorf("%w: %s resource needs a key", domain.ErrInvalidArgument, t)
	}

	switch t {
	case domain.ResourceRepo:
		return Resource{Type: t, Key: normalizeSimple(key)}, nil

	case domain.ResourcePath, domain.ResourceLockfile, domain.ResourceTest:
		p, err := normalizePath(key)
		if err != nil {
			return Resource{}, err
		}
		return Resource{Type: t, Key: p}, nil

	case domain.ResourceDir:
		p, err := normalizePath(key)
		if err != nil {
			return Resource{}, err
		}
		// The repository root is a legitimate directory reservation; it normalizes to ".".
		return Resource{Type: t, Key: p}, nil

	case domain.ResourceMigration:
		// Migration lanes are opaque labels. "migration:*" and "migration:primary" are both
		// legal; the wildcard is handled in Overlaps.
		return Resource{Type: t, Key: normalizeSimple(key)}, nil

	case domain.ResourceSchema, domain.ResourceSymbol:
		return Resource{Type: t, Key: normalizeSimple(key)}, nil

	case domain.ResourceAPI:
		return Resource{Type: t, Key: normalizeAPI(key)}, nil

	default:
		return Resource{}, fmt.Errorf("%w: resource_type=%q", domain.ErrInvalidEnum, t)
	}
}

// normalizeSimple applies Unicode NFC and trims decoration, so visually identical keys
// compare equal (DESIGN.md §11.4).
func normalizeSimple(s string) string {
	return norm.NFC.String(strings.TrimSpace(s))
}

// normalizePath resolves a repository-relative path to its canonical form and refuses to
// escape the repository root.
func normalizePath(p string) (string, error) {
	p = norm.NFC.String(strings.TrimSpace(p))
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return ".", nil
	}

	// path.Clean collapses "a/../b" and repeated separators. Traversal that survives it has
	// escaped the root, which is never a legal reservation.
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: path escapes repository root: %q", domain.ErrInvalidArgument, p)
	}
	return cleaned, nil
}

// normalizeAPI canonicalizes "METHOD /route" into "METHOD /route" with an uppercase method
// and a leading slash, so "get /v1/tasks" and "GET /v1/tasks" are one resource.
func normalizeAPI(s string) string {
	s = normalizeSimple(s)
	method, route, found := strings.Cut(s, " ")
	if !found {
		return s
	}
	route = strings.TrimSpace(route)
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	return strings.ToUpper(strings.TrimSpace(method)) + " " + route
}

// Overlaps reports whether two resources address any of the same underlying thing.
//
// Ambiguity resolves to `true`. A false negative here means two agents silently edit the
// same file; a false positive means someone is asked to coordinate unnecessarily. Those
// costs are not symmetric.
func Overlaps(a, b Resource) bool {
	// A repo-wide reservation covers everything in that repository.
	if a.Type == domain.ResourceRepo || b.Type == domain.ResourceRepo {
		return true
	}

	// Migrations serialize project-wide: any two migration lanes conflict unless they are
	// explicitly different named lanes.
	if a.Type == domain.ResourceMigration || b.Type == domain.ResourceMigration {
		if a.Type != b.Type {
			// A migration touches schema and the files that define it, so it overlaps
			// schema resources and anything under a migrations directory.
			other := b
			if b.Type == domain.ResourceMigration {
				other = a
			}
			if other.Type == domain.ResourceSchema {
				return true
			}
			if other.PathLike() && looksLikeMigrationPath(other.Key) {
				return true
			}
			return false
		}
		if a.Key == "*" || b.Key == "*" {
			return true
		}
		return a.Key == b.Key
	}

	if a.Type == domain.ResourceSchema || b.Type == domain.ResourceSchema {
		if a.Type != b.Type {
			return false
		}
		return a.Key == b.Key
	}

	if a.Type == domain.ResourceAPI || b.Type == domain.ResourceAPI {
		if a.Type != b.Type {
			return false
		}
		return apiOverlaps(a.Key, b.Key)
	}

	if a.Type == domain.ResourceSymbol || b.Type == domain.ResourceSymbol {
		if a.Type == b.Type {
			return symbolOverlaps(a.Key, b.Key)
		}
		// A symbol lives in a file; if the counterpart is path-like, compare the symbol's
		// file component against it.
		sym, other := a, b
		if b.Type == domain.ResourceSymbol {
			sym, other = b, a
		}
		if !other.PathLike() {
			return false
		}
		file := symbolFile(sym.Key)
		if file == "" {
			return false
		}
		return pathOverlaps(file, domain.ResourcePath, other.Key, other.Type)
	}

	// Everything remaining is path-like: path, dir, lockfile, test.
	return pathOverlaps(a.Key, a.Type, b.Key, b.Type)
}

func looksLikeMigrationPath(p string) bool {
	lower := strings.ToLower(p)
	for _, marker := range []string{"migration", "migrate", "schema"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// pathOverlaps compares two path-like resources, honouring directory containment and glob
// patterns.
func pathOverlaps(aKey string, aType domain.ResourceType, bKey string, bType domain.ResourceType) bool {
	aGlob, bGlob := hasMeta(aKey), hasMeta(bKey)

	switch {
	case aGlob && bGlob:
		// Two patterns. Deciding whether two glob languages intersect is not tractable in
		// general, so use a conservative approximation: they overlap unless their literal
		// suffixes prove they select different file kinds, or their literal prefixes prove
		// they select different subtrees.
		if aKey == bKey {
			return true
		}
		if !suffixesCompatible(aKey, bKey) {
			return false
		}
		ap, bp := literalPrefix(aKey), literalPrefix(bKey)
		return isUnder(ap, bp) || isUnder(bp, ap)

	case aGlob:
		return globCoversConcrete(aKey, bKey, bType)

	case bGlob:
		return globCoversConcrete(bKey, aKey, aType)
	}

	aDir := aType == domain.ResourceDir
	bDir := bType == domain.ResourceDir

	switch {
	case aDir && bDir:
		return isUnder(aKey, bKey) || isUnder(bKey, aKey)
	case aDir:
		return isUnder(bKey, aKey)
	case bDir:
		return isUnder(aKey, bKey)
	default:
		return aKey == bKey
	}
}

// globCoversConcrete reports whether pattern matches a concrete resource. For a directory,
// the pattern also matches if it targets anything beneath it.
func globCoversConcrete(pattern, key string, keyType domain.ResourceType) bool {
	if matchPattern(pattern, key) {
		return true
	}
	if keyType == domain.ResourceDir {
		// "internal/**/*.go" overlaps "dir:internal/api" because files under that directory
		// can match. Compare the pattern's literal prefix against the directory.
		prefix := literalPrefix(pattern)
		return isUnder(prefix, key) || isUnder(key, prefix)
	}
	return false
}

// isUnder reports whether p is dir itself or lies beneath it.
func isUnder(p, dir string) bool {
	if dir == "." || dir == "" {
		return true
	}
	if p == dir {
		return true
	}
	return strings.HasPrefix(p, dir+"/")
}

func hasMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// suffixesCompatible reports whether two patterns could select the same file, judged by the
// literal text after their last metacharacter. "**/*.go" and "**/*.md" cannot; "**/*.go" and
// "**/*_test.go" can.
func suffixesCompatible(a, b string) bool {
	as, bs := literalSuffix(a), literalSuffix(b)
	if as == "" || bs == "" {
		return true
	}
	return strings.HasSuffix(as, bs) || strings.HasSuffix(bs, as)
}

// literalSuffix returns the trailing text of a pattern that contains no metacharacters.
func literalSuffix(pattern string) string {
	if i := strings.LastIndexAny(pattern, "*?]"); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// literalPrefix returns the leading path segments of a pattern that contain no metacharacters.
func literalPrefix(pattern string) string {
	segs := strings.Split(pattern, "/")
	var out []string
	for _, s := range segs {
		if hasMeta(s) {
			break
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "."
	}
	return strings.Join(out, "/")
}

// matchPattern is path.Match extended with `**`, which crosses separators.
//
// path.Match alone is not enough: `internal/**/*_test.go` is the natural way to express a
// reservation over a subtree, and stdlib matching stops at `/`.
func matchPattern(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, name)
		return err == nil && ok
	}
	return matchDoubleStar(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// matchDoubleStar matches pattern segments against name segments, where a "**" segment
// consumes zero or more name segments.
func matchDoubleStar(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trailing "**" matches whatever remains, including nothing.
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchDoubleStar(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// apiOverlaps compares "METHOD /route" keys. `*` matches any method, and `:param`/`{param}`
// segments match any single path segment.
func apiOverlaps(a, b string) bool {
	aMethod, aRoute := splitAPI(a)
	bMethod, bRoute := splitAPI(b)

	if aMethod != "*" && bMethod != "*" && aMethod != bMethod {
		return false
	}
	return routeOverlaps(aRoute, bRoute)
}

func splitAPI(s string) (method, route string) {
	method, route, found := strings.Cut(s, " ")
	if !found {
		return "*", s
	}
	return strings.ToUpper(method), route
}

func routeOverlaps(a, b string) bool {
	as := strings.Split(strings.Trim(a, "/"), "/")
	bs := strings.Split(strings.Trim(b, "/"), "/")
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if isRouteParam(as[i]) || isRouteParam(bs[i]) {
			continue
		}
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func isRouteParam(seg string) bool {
	return strings.HasPrefix(seg, ":") ||
		(strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")) ||
		seg == "*"
}

// symbolOverlaps compares language-qualified symbols. A package-level reservation
// ("go:pkg/api") covers the symbols inside it ("go:pkg/api.Handler").
func symbolOverlaps(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+".") || strings.HasPrefix(b, a+".")
}

// symbolFile extracts a file path from a symbol key of the form "path/to/file.go:Symbol" or
// "internal/ledger.Claim". Returns "" when no file component is recoverable.
func symbolFile(key string) string {
	if file, _, found := strings.Cut(key, ":"); found && strings.Contains(file, "/") {
		return file
	}
	if idx := strings.LastIndex(key, "."); idx > 0 {
		candidate := key[:idx]
		if strings.Contains(candidate, "/") {
			return candidate
		}
	}
	return ""
}

// Set is a small helper for de-duplicating resources while preserving insertion order,
// which keeps API responses stable across calls.
type Set struct {
	seen  map[string]bool
	order []Resource
}

func NewSet() *Set { return &Set{seen: map[string]bool{}} }

func (s *Set) Add(r Resource) bool {
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	k := r.String()
	if s.seen[k] {
		return false
	}
	s.seen[k] = true
	s.order = append(s.order, r)
	return true
}

func (s *Set) All() []Resource { return s.order }

func (s *Set) Len() int { return len(s.order) }

// Strings renders the set as canonical resource strings.
func (s *Set) Strings() []string {
	out := make([]string, 0, len(s.order))
	for _, r := range s.order {
		out = append(out, r.String())
	}
	return out
}
