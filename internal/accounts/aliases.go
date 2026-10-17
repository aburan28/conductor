package accounts

// Aliases are second names for accounts, per harness. An alias points at an account
// ("acme" → "work") or straight at a state directory ("old" → ~/.claude-legacy), and every
// command that takes an account name takes an alias in its place. A project bound to an
// alias follows it, so re-pointing one alias moves every project that uses it.
//
// Aliases live in ~/.conductor/accounts/aliases.json, machine-local like the bindings. They
// do not chain: an alias names a real account or a directory, never another alias, so a
// lookup is one step and cannot loop.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Alias is one second name for one harness's account.
type Alias struct {
	Harness string `json:"harness"`
	Alias   string `json:"alias"`
	Target  string `json:"target"` // an account name, or an absolute directory
}

// IsDir reports whether the alias points at a directory rather than an account name.
func (a Alias) IsDir() bool { return filepath.IsAbs(a.Target) }

type aliasFile map[string]map[string]string // harness → alias → target

// SetAlias makes alias another name for target: an account name, or a directory path
// (absolute, relative, or ~/…), which then serves as an isolated account's state directory.
func SetAlias(harness, alias, target string) error {
	if !knownHarness(harness) {
		return fmt.Errorf("unknown harness %q", harness)
	}
	if err := validName(alias); err != nil {
		return err
	}
	if exists(harness, alias) {
		return fmt.Errorf("%s already has an account named %q; an alias cannot shadow it", harness, alias)
	}
	all, err := readAliases()
	if err != nil {
		return err
	}
	if looksLikePath(target) {
		dir, err := expandPath(target)
		if err != nil {
			return err
		}
		target = dir
	} else {
		if err := validName(target); err != nil {
			return err
		}
		if target == alias {
			return fmt.Errorf("alias %q cannot point at itself", alias)
		}
		if _, isAlias := all[harness][target]; isAlias {
			return fmt.Errorf("%q is itself an alias; point %q at the account it names", target, alias)
		}
	}
	// Re-pointing an alias is fine; making an existing alias's target an alias is not.
	for other, t := range all[harness] {
		if t == alias && other != alias {
			return fmt.Errorf("alias %q points at %q; aliases do not chain", other, alias)
		}
	}
	if all[harness] == nil {
		all[harness] = map[string]string{}
	}
	all[harness][alias] = target
	return writeAliases(all)
}

// RemoveAlias forgets an alias. Projects bound to it fall back to an account of that name,
// if one exists. Removing an alias that does not exist is an error, so a typo is not silent.
func RemoveAlias(harness, alias string) error {
	all, err := readAliases()
	if err != nil {
		return err
	}
	if _, ok := all[harness][alias]; !ok {
		return fmt.Errorf("%s has no alias %q", harness, alias)
	}
	delete(all[harness], alias)
	if len(all[harness]) == 0 {
		delete(all, harness)
	}
	return writeAliases(all)
}

// Aliases lists a harness's aliases (every harness's when harness is empty), ordered by
// harness then alias.
func Aliases(harness string) ([]Alias, error) {
	all, err := readAliases()
	if err != nil {
		return nil, err
	}
	var out []Alias
	for h, m := range all {
		if harness != "" && h != harness {
			continue
		}
		for a, t := range m {
			out = append(out, Alias{Harness: h, Alias: a, Target: t})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Harness != out[j].Harness {
			return out[i].Harness < out[j].Harness
		}
		return out[i].Alias < out[j].Alias
	})
	return out, nil
}

// resolveAlias turns a name into the account it means. dir is set when the alias points
// at a directory; otherwise name is the real account name (itself, when it is no alias).
// An unreadable alias file resolves nothing rather than failing every account command.
func resolveAlias(harness, name string) (canonical, dir string) {
	all, err := readAliases()
	if err != nil {
		return name, ""
	}
	target, ok := all[harness][name]
	if !ok {
		return name, ""
	}
	if filepath.IsAbs(target) {
		return name, target
	}
	return target, ""
}

// canonical is resolveAlias for the snapshot commands, which need an account name: an
// alias for a directory has no credential snapshot to switch to.
func canonical(harness, name string) (string, error) {
	real, dir := resolveAlias(harness, name)
	if dir != "" {
		return "", fmt.Errorf("%q is an alias for the directory %s, not a saved %s login", name, dir, harness)
	}
	return real, nil
}

func isAlias(harness, name string) bool {
	all, err := readAliases()
	if err != nil {
		return false
	}
	_, ok := all[harness][name]
	return ok
}

// exists reports whether harness has a real account (snapshot or isolated) by this name.
func exists(harness, name string) bool {
	root, err := Root()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, harness, name+".json")); err == nil {
		return true
	}
	return isDir(filepath.Join(root, harness, name))
}

func knownHarness(harness string) bool {
	if _, ok := isolation[harness]; ok {
		return true
	}
	_, ok := livePaths[harness]
	return ok
}

func looksLikePath(s string) bool {
	return strings.ContainsAny(s, `/\`) || s == "~" || s == "." || s == ".."
}

func expandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

func aliasesPath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "aliases.json"), nil
}

func readAliases() (aliasFile, error) {
	path, err := aliasesPath()
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return aliasFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	all := aliasFile{}
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return all, nil
}

func writeAliases(all aliasFile) error {
	path, err := aliasesPath()
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(body, '\n'))
}
