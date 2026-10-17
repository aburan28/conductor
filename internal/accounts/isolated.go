package accounts

// Isolated accounts are the other way to run more than one login: instead of copying
// credentials over the harness's single live file, each account gets a state directory of
// its own and the harness is pointed at it. Claude Code reads CLAUDE_CONFIG_DIR, Codex reads
// CODEX_HOME, OpenCode follows XDG_DATA_HOME, and Cursor (a VS Code build) takes
// --user-data-dir. Nothing is swapped, so two projects can run under two accounts at the same
// time on one machine.
//
// A project is bound to an account per harness, machine-locally: which login a person uses
// for a repository is their own business, not repository policy, so the binding lives in
// ~/.conductor/accounts/bindings.json keyed by the checkout's root, never in .conductor/.
// `conductor wrap` applies it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// isolation says how each harness is pointed at a state directory: an environment variable,
// or (Cursor) a command-line flag.
var isolation = map[string]struct{ env, flag string }{
	"claude":   {env: "CLAUDE_CONFIG_DIR"},
	"codex":    {env: "CODEX_HOME"},
	"opencode": {env: "XDG_DATA_HOME"},
	"cursor":   {flag: "--user-data-dir"},
}

// IsolatedHarnesses lists the harnesses that can run from a state directory of their own.
func IsolatedHarnesses() []string {
	return []string{"claude", "codex", "cursor", "opencode"}
}

// StateDir is where an isolated account keeps its state. The community convention for
// Claude Code is ~/.claude-<name>, so an existing ~/.<harness>-<name> is honoured; otherwise
// the directory is ~/.conductor/accounts/<harness>/<name> (beside, not inside, the
// credential snapshots, which are <name>.json files).
func StateDir(harness, name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	if os.Getenv("CONDUCTOR_STATE_DIR") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			if p := filepath.Join(home, "."+harness+"-"+name); isDir(p) {
				return p, nil
			}
		}
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, harness, name), nil
}

// Isolated returns the names of a harness's isolated accounts under ~/.conductor/accounts.
func Isolated(harness string) ([]string, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, harness))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Isolation is what a launch needs to run a harness under an isolated account.
type Isolation struct {
	Harness string
	Account string
	Dir     string
	Env     string // the variable to set to Dir; empty when the harness takes a flag
	Args    []string
	Source  string // "CONDUCTOR_ACCOUNT" or "binding"
}

// Prepare creates an isolated account's directory (0700: it will hold credentials) and
// says how to launch the harness under it.
func Prepare(harness, name string) (Isolation, error) {
	how, ok := isolation[harness]
	if !ok {
		return Isolation{}, fmt.Errorf("%s cannot run from an isolated account (%s)",
			harness, strings.Join(IsolatedHarnesses(), ", "))
	}
	dir, err := StateDir(harness, name)
	if err != nil {
		return Isolation{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Isolation{}, err
	}
	iso := Isolation{Harness: harness, Account: name, Dir: dir, Env: how.env}
	if how.flag != "" {
		iso.Args = []string{how.flag, dir}
	}
	return iso, nil
}

// Resolve decides which isolated account a launch of harness in projectRoot runs under:
// CONDUCTOR_ACCOUNT (one launch, any project) wins over the project's binding. Ok is false
// when neither names one, and the harness runs under its default login.
func Resolve(harness, projectRoot string, getenv func(string) string) (Isolation, bool, error) {
	if _, ok := isolation[harness]; !ok {
		return Isolation{}, false, nil
	}
	name, source := getenv("CONDUCTOR_ACCOUNT"), "CONDUCTOR_ACCOUNT"
	if name == "" {
		bound, err := Bound(projectRoot, harness)
		if err != nil {
			return Isolation{}, false, err
		}
		name, source = bound, "binding"
	}
	if name == "" {
		return Isolation{}, false, nil
	}
	iso, err := Prepare(harness, name)
	if err != nil {
		return Isolation{}, false, err
	}
	iso.Source = source
	return iso, true, nil
}

// bindings maps a checkout root to {harness: account}.
type bindings map[string]map[string]string

// Bind makes harness run under the isolated account name whenever it is wrapped in
// projectRoot. An empty name removes the binding.
func Bind(projectRoot, harness, name string) error {
	if _, ok := isolation[harness]; !ok {
		return fmt.Errorf("%s cannot run from an isolated account (%s)",
			harness, strings.Join(IsolatedHarnesses(), ", "))
	}
	if name != "" {
		if err := validName(name); err != nil {
			return err
		}
	}
	key, err := projectKey(projectRoot)
	if err != nil {
		return err
	}
	b, err := readBindings()
	if err != nil {
		return err
	}
	if name == "" {
		delete(b[key], harness)
		if len(b[key]) == 0 {
			delete(b, key)
		}
	} else {
		if b[key] == nil {
			b[key] = map[string]string{}
		}
		b[key][harness] = name
	}
	return writeBindings(b)
}

// Bound names the account harness is bound to in projectRoot; empty when none is.
func Bound(projectRoot, harness string) (string, error) {
	key, err := projectKey(projectRoot)
	if err != nil {
		return "", err
	}
	b, err := readBindings()
	if err != nil {
		return "", err
	}
	return b[key][harness], nil
}

// Binding is one project's account for one harness.
type Binding struct {
	Project string `json:"project"`
	Harness string `json:"harness"`
	Account string `json:"account"`
}

// Bindings lists every project binding on this machine, ordered by project then harness.
func Bindings() ([]Binding, error) {
	b, err := readBindings()
	if err != nil {
		return nil, err
	}
	var out []Binding
	for project, m := range b {
		for harness, account := range m {
			out = append(out, Binding{Project: project, Harness: harness, Account: account})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Harness < out[j].Harness
	})
	return out, nil
}

func bindingsPath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "bindings.json"), nil
}

func readBindings() (bindings, error) {
	path, err := bindingsPath()
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return bindings{}, nil
	}
	if err != nil {
		return nil, err
	}
	b := bindings{}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return b, nil
}

func writeBindings(b bindings) error {
	path, err := bindingsPath()
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(body, '\n'))
}

// projectKey is the checkout root as an absolute, symlink-free path, so the same checkout
// reached through two paths (macOS's /tmp → /private/tmp) has one binding.
func projectKey(root string) (string, error) {
	if root == "" {
		return "", errors.New("no project directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

// validName keeps an account name a single path element: it becomes a directory name.
func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) ||
		strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid account name %q: use a plain name like work or personal", name)
	}
	return nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
