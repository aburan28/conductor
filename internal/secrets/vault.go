// Package secrets is the project secret vault: small credentials sealed at rest under
// conductord's secretbox key and retrieved by agents (MCP) or local tools (CLI / wrap).
//
// Values never appear in list responses, task metadata, or SSE — only a short hint is kept
// in the clear. Opening a secret is audited without recording the plaintext.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/secretbox"
)

// ErrNotConfigured means the server has no secretbox key source for the vault.
var ErrNotConfigured = errors.New("secret vault is not configured on this server")

const (
	// MaxSecretsPerProject bounds how many named secrets one project may hold.
	MaxSecretsPerProject = 100
	// MaxValueBytes is the largest plaintext a secret may hold.
	MaxValueBytes = 64 << 10
	// MaxNameLen / MaxDescriptionLen bound metadata fields.
	MaxNameLen        = 128
	MaxDescriptionLen = 500
)

// sealPurpose binds ciphertext to a project so a sealed blob cannot be copied across projects.
func sealPurpose(projectID domain.ID) string {
	return "conductor/project_secret/" + string(projectID)
}

// Portable shell identifier. Dots and dashes are not valid in `export` / `set -gx`,
// so a name that passed a looser check would abort `eval "$(conductor secrets env)"`.
var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedNames and reservedPrefixes are environment variables a project secret may never
// be named. Every secret is exported into contributors' shells (`secrets inject`), the child
// environment of `conductor wrap`, and ~/.claude/settings.json, so a maintainer — or anyone
// holding a maintainer's token — who could name one PATH, BASH_ENV, LD_PRELOAD,
// GIT_CONFIG_COUNT or NODE_OPTIONS would be running code on every contributor's machine
// rather than handing them a credential. The list covers what a loader, a shell, git, a
// language runtime, or Conductor itself reads to decide what to execute, where to find
// configuration, or whom to trust. Matching ignores case: Windows environments do.
var reservedNames = map[string]bool{
	// Shells and the process environment.
	"PATH": true, "HOME": true, "SHELL": true, "USER": true, "LOGNAME": true, "ENV": true,
	"BASH_ENV": true, "SHELLOPTS": true, "BASHOPTS": true, "IFS": true, "CDPATH": true,
	"PROMPT_COMMAND": true, "PS0": true, "PS1": true, "PS2": true, "PS3": true, "PS4": true,
	"PROMPT": true, "RPROMPT": true, "ZDOTDIR": true, "GLOBIGNORE": true, "TMPDIR": true,
	"GLIBC_TUNABLES": true,
	// Programs other tools launch on the user's behalf.
	"EDITOR": true, "VISUAL": true, "PAGER": true, "MANPAGER": true, "BROWSER": true,
	"LESSOPEN": true, "LESSCLOSE": true, "SSH_ASKPASS": true, "SUDO_ASKPASS": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CONFIG_DIRS": true,
	// git: anything that names a program, a repository, or configuration.
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_EXEC_PATH": true, "GIT_TEMPLATE_DIR": true,
	"GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_ASKPASS": true, "GIT_EDITOR": true,
	"GIT_SEQUENCE_EDITOR": true, "GIT_PAGER": true, "GIT_EXTERNAL_DIFF": true,
	"GIT_PROXY_COMMAND": true,
	// Language runtimes that load code or options from the environment.
	"NODE_OPTIONS": true, "NODE_PATH": true, "PYTHONPATH": true, "PYTHONSTARTUP": true,
	"PYTHONHOME": true, "PERL5OPT": true, "PERL5LIB": true, "PERLLIB": true, "RUBYOPT": true,
	"RUBYLIB": true, "JAVA_TOOL_OPTIONS": true, "_JAVA_OPTIONS": true, "JDK_JAVA_OPTIONS": true,
	"CLASSPATH": true, "GOFLAGS": true,
	// Trust anchors and proxies: a substituted CA or proxy sees every credential in transit.
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true,
	"REQUESTS_CA_BUNDLE": true, "NODE_EXTRA_CA_CERTS": true, "GIT_SSL_CAINFO": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	// Harness configuration roots.
	"CLAUDE_CONFIG_DIR": true, "CODEX_HOME": true,
}

var reservedPrefixes = []string{
	"LD_", "DYLD_", // dynamic loader: LD_PRELOAD, LD_LIBRARY_PATH, DYLD_INSERT_LIBRARIES
	"BASH_FUNC_",  // exported shell functions
	"GIT_CONFIG",  // GIT_CONFIG, _COUNT, _KEY_n, _VALUE_n, _GLOBAL, _SYSTEM, _PARAMETERS
	"NPM_CONFIG_", // npm reads every option (script-shell, …) from these
	"CONDUCTOR_",  // the CLI's own endpoint, token, and session identity
}

// ReservedName reports whether name is an environment variable no project secret may use
// (see reservedNames). The vault refuses to create one, and every client path that exports
// secrets into an environment drops one, so a row stored before the rule cannot bite.
func ReservedName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if reservedNames[upper] {
		return true
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}

// Vault seals and opens project secrets.
type Vault struct {
	store *db.Store
	key   *secretbox.Source
}

// New returns a Vault. Without a SecretKey, writes fail and reads cannot decrypt.
func New(store *db.Store, key *secretbox.Source) *Vault {
	return &Vault{store: store, key: key}
}

// View is a secret as list/get-metadata APIs return it: never the plaintext.
type View struct {
	Name        string    `json:"name"`
	Hint        string    `json:"hint"`
	Description string    `json:"description,omitempty"`
	CreatedBy   domain.ID `json:"created_by,omitempty"`
	UpdatedBy   domain.ID `json:"updated_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func viewOf(s db.ProjectSecret) View {
	return View{
		Name: s.Name, Hint: s.Hint, Description: s.Description,
		CreatedBy: s.CreatedBy, UpdatedBy: s.UpdatedBy,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

// SetRequest is a paste/upsert of one secret.
type SetRequest struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// Opened is a decrypted secret returned once to the caller (MCP / CLI get / wrap env).
type Opened struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Hint  string `json:"hint,omitempty"`
}

// ValidateName checks a secret name looks like an environment variable.
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: name is required", domain.ErrInvalidArgument)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%w: name is at most %d characters", domain.ErrInvalidArgument, MaxNameLen)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: name must look like an environment variable (letters, digits, and _)", domain.ErrInvalidArgument)
	}
	// GET /v1/projects/{project}/secrets/export is the bulk plaintext route. A secret
	// named "export" would be stored by POST but unreachable via GET-by-name.
	if name == "export" {
		return fmt.Errorf("%w: name %q is reserved", domain.ErrInvalidArgument, name)
	}
	return nil
}

// ShellName reports whether name is a portable shell identifier. "export" is a legal
// identifier and is rejected separately by ValidateName because it shadows the HTTP route.
func ShellName(name string) bool {
	return nameRe.MatchString(name)
}

// HintFromValue returns a short non-secret reminder (last 4 runes when printable).
func HintFromValue(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= 4 {
		return "****"
	}
	tail := string(runes[len(runes)-4:])
	for _, r := range tail {
		if r < 32 || r == 127 {
			return "****"
		}
	}
	return "…" + tail
}

// Set seals and stores a secret (maintainer path).
func (v *Vault) Set(ctx context.Context, project domain.Project, by domain.ID, req SetRequest) (View, error) {
	if v == nil || v.key == nil {
		return View{}, ErrNotConfigured
	}
	name := strings.TrimSpace(req.Name)
	if err := ValidateName(name); err != nil {
		return View{}, err
	}
	// Only creation is refused: an existing row with a reserved name can still be opened by
	// name and deleted, and Export leaves it out.
	if ReservedName(name) {
		return View{}, fmt.Errorf("%w: %q is an environment variable that controls how programs run; a secret exported under it would be code execution on every contributor's machine", domain.ErrInvalidArgument, name)
	}
	desc := strings.TrimSpace(req.Description)
	if utf8.RuneCountInString(desc) > MaxDescriptionLen {
		return View{}, fmt.Errorf("%w: description is at most %d characters", domain.ErrInvalidArgument, MaxDescriptionLen)
	}
	if req.Value == "" {
		return View{}, fmt.Errorf("%w: value is required", domain.ErrInvalidArgument)
	}
	if len(req.Value) > MaxValueBytes {
		return View{}, fmt.Errorf("%w: value is at most %d bytes", domain.ErrInvalidArgument, MaxValueBytes)
	}

	existing, err := v.store.GetProjectSecret(ctx, project.ID, name)
	if err != nil && err != domain.ErrNotFound {
		return View{}, err
	}
	if err == domain.ErrNotFound {
		n, err := v.store.CountProjectSecrets(ctx, project.ID)
		if err != nil {
			return View{}, err
		}
		if n >= MaxSecretsPerProject {
			return View{}, fmt.Errorf("%w: at most %d secrets per project", domain.ErrInvalidArgument, MaxSecretsPerProject)
		}
	} else {
		_ = existing
	}

	key, err := v.key.Get(true)
	if err != nil {
		return View{}, err
	}
	sealed, err := key.Seal([]byte(req.Value), sealPurpose(project.ID))
	if err != nil {
		return View{}, err
	}
	sec, err := v.store.UpsertProjectSecret(ctx, db.ProjectSecret{
		OrganizationID: project.OrganizationID,
		ProjectID:      project.ID,
		Name:           name,
		ValueSealed:    sealed,
		Hint:           HintFromValue(req.Value),
		Description:    desc,
		UpdatedBy:      by,
	})
	if err != nil {
		return View{}, err
	}
	return viewOf(sec), nil
}

// List returns metadata only.
func (v *Vault) List(ctx context.Context, projectID domain.ID) ([]View, error) {
	rows, err := v.store.ListProjectSecrets(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, r := range rows {
		out = append(out, viewOf(r))
	}
	return out, nil
}

// Open decrypts one secret for the caller.
func (v *Vault) Open(ctx context.Context, project domain.Project, name string) (Opened, error) {
	if v == nil || v.key == nil {
		return Opened{}, ErrNotConfigured
	}
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return Opened{}, err
	}
	sec, err := v.store.GetProjectSecret(ctx, project.ID, name)
	if err != nil {
		return Opened{}, err
	}
	key, err := v.key.Get(false)
	if err != nil {
		return Opened{}, err
	}
	plain, err := key.Open(sec.ValueSealed, sealPurpose(project.ID))
	if err != nil {
		return Opened{}, err
	}
	return Opened{Name: sec.Name, Value: string(plain), Hint: sec.Hint}, nil
}

// Export decrypts every secret for injection into a local process environment.
func (v *Vault) Export(ctx context.Context, project domain.Project) (map[string]string, error) {
	if v == nil || v.key == nil {
		return nil, ErrNotConfigured
	}
	rows, err := v.store.ListProjectSecrets(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return map[string]string{}, nil
	}
	key, err := v.key.Get(false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	purpose := sealPurpose(project.ID)
	for _, sec := range rows {
		if ReservedName(sec.Name) {
			continue // stored before the rule; never handed to an environment (ReservedName)
		}
		plain, err := key.Open(sec.ValueSealed, purpose)
		if err != nil {
			return nil, fmt.Errorf("unseal %s: %w", sec.Name, err)
		}
		out[sec.Name] = string(plain)
	}
	return out, nil
}

// Delete removes a secret.
func (v *Vault) Delete(ctx context.Context, projectID domain.ID, name string) error {
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return err
	}
	return v.store.DeleteProjectSecret(ctx, projectID, name)
}
