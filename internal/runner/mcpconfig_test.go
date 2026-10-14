package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/harness"
)

func readServers(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.MCPServers
}

func runnerWith(servers map[string]map[string]harness.MCPServer) *Runner {
	return &Runner{opts: Options{
		MCPEndpoint:       "http://localhost:8080",
		MCPCommand:        "conductor-mcp",
		HarnessMCPServers: servers,
	}}
}

func TestWriteMCPConfigMergesHarnessServers(t *testing.T) {
	r := runnerWith(map[string]map[string]harness.MCPServer{
		"cairn": {"cairn": {
			Command: "/bin/cairn",
			Args:    []string{"--log", "/l.jsonl", "mcp"},
			Env:     map[string]string{"CAIRN_EPOCH_SECONDS": "1"},
		}},
	})

	path, err := r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "cairn", "t")
	if err != nil {
		t.Fatal(err)
	}
	servers := readServers(t, path)

	// The harness's server is present, with its arguments and environment intact...
	cairn, ok := servers["cairn"]
	if !ok {
		t.Fatalf("cairn server missing: %v", servers)
	}
	if cairn["command"] != "/bin/cairn" {
		t.Errorf("command = %v", cairn["command"])
	}
	if args, _ := cairn["args"].([]any); len(args) != 3 || args[2] != "mcp" {
		t.Errorf("args = %v", cairn["args"])
	}
	// ...and conductor's own channel is still there beside it.
	if _, ok := servers["conductor"]; !ok {
		t.Error("conductor server missing; the agent cannot report progress")
	}

	// A harness that declares nothing gets the conductor server alone.
	path, err = r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "claude", "t")
	if err != nil {
		t.Fatal(err)
	}
	if servers := readServers(t, path); len(servers) != 1 || servers["conductor"] == nil {
		t.Errorf("undeclared harness got %v, want conductor alone", servers)
	}
}

// A harness config is repository data. If it could define a server named "conductor", an
// agent could be pointed at a coordination channel nobody is reading.
func TestWriteMCPConfigRefusesToShadowConductor(t *testing.T) {
	r := runnerWith(map[string]map[string]harness.MCPServer{
		"cairn": {"conductor": {Command: "/bin/impostor"}},
	})

	path, err := r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "cairn", "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := readServers(t, path)["conductor"]["command"]; got != "conductor-mcp" {
		t.Errorf("conductor command = %v, want the runner's own", got)
	}
}

func TestWriteMCPConfigSkippedWithoutEndpoint(t *testing.T) {
	r := &Runner{opts: Options{}}
	path, err := r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "cairn", "t")
	if err != nil || path != "" {
		t.Errorf("path = %q, err = %v; want no config written", path, err)
	}
}

// tokenBackend records the per-attempt credential traffic; every other Backend method is
// unused by prepareMCP and panics if called.
type tokenBackend struct {
	Backend
	minted, revoked []string
	ttl             time.Duration
	mintErr         error
}

func (b *tokenBackend) MintAttemptToken(_ context.Context, attemptID domain.ID, ttl time.Duration) (string, string, error) {
	if b.mintErr != nil {
		return "", "", b.mintErr
	}
	b.ttl = ttl
	name := attemptTokenName(attemptID)
	b.minted = append(b.minted, name)
	return "cdt_attempt_scoped", name, nil
}

func (b *tokenBackend) RevokeToken(_ context.Context, name string) error {
	b.revoked = append(b.revoked, name)
	return nil
}

// The agent's credential is minted for its attempt, never copied from the runner's own login,
// lives outside the worktree the runner commits with `git add -A`, and dies with the attempt.
func TestPrepareMCPUsesAPerAttemptCredentialOutsideTheWorktree(t *testing.T) {
	backend := &tokenBackend{}
	worktree := t.TempDir()
	r := &Runner{backend: backend, opts: Options{
		MCPEndpoint: "http://localhost:8080", MCPCommand: "conductor-mcp",
		AttemptTimeout: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	claim := db.ClaimResult{
		Task:    domain.Task{Ref: "T-1"},
		Attempt: domain.Attempt{ID: "att-1"},
		Fence:   domain.Fence{TaskID: "task-1", AttemptID: "att-1", LeaseID: "lease-1", FencingEpoch: 3},
	}

	path, cleanup, err := r.prepareMCP(context.Background(), domain.Project{ID: "p"}, claim, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(path, worktree) {
		t.Errorf("MCP config %s is inside the worktree", path)
	}
	if len(backend.minted) != 1 || backend.minted[0] != "attempt:att-1" {
		t.Errorf("minted %v, want one attempt credential", backend.minted)
	}
	if backend.ttl <= time.Hour || backend.ttl > 2*time.Hour {
		t.Errorf("credential ttl = %s, want just past the attempt timeout", backend.ttl)
	}
	env, _ := readServers(t, path)["conductor"]["env"].(map[string]any)
	if env["CONDUCTOR_TOKEN"] != "cdt_attempt_scoped" {
		t.Errorf("agent token = %v, want the per-attempt credential", env["CONDUCTOR_TOKEN"])
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("config directory mode = %v (err %v), want 0700", info.Mode().Perm(), err)
	}

	cleanup()
	if len(backend.revoked) != 1 || backend.revoked[0] != "attempt:att-1" {
		t.Errorf("revoked %v, want the attempt credential revoked at the end", backend.revoked)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("MCP config survived cleanup: %v", err)
	}
}

// Without a credential the agent gets no conductor server at all — never the runner's own.
func TestPrepareMCPWithoutACredentialOmitsConductor(t *testing.T) {
	backend := &tokenBackend{mintErr: domain.ErrNotPermitted}
	r := &Runner{backend: backend, opts: Options{
		MCPEndpoint: "http://localhost:8080", MCPCommand: "conductor-mcp",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	path, cleanup, err := r.prepareMCP(context.Background(), domain.Project{ID: "p"},
		db.ClaimResult{Attempt: domain.Attempt{ID: "att-2"}}, "claude")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, ok := readServers(t, path)["conductor"]; ok {
		t.Error("conductor server written without an attempt credential")
	}
}

// A required check runs whatever the agent left in the worktree, so it must not see the
// runner's credentials.
func TestRunChecksDoNotInheritCredentials(t *testing.T) {
	t.Setenv("CONDUCTOR_TOKEN", "cdt_operator")
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	r := &Runner{opts: Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	results := r.runChecks(context.Background(), t.TempDir(), []string{
		`test -z "$CONDUCTOR_TOKEN" && test -z "$DATABASE_URL" && test -z "$ANTHROPIC_API_KEY" && test -n "$PATH"`,
	})
	if len(results) != 1 || results[0].ExitCode != 0 {
		t.Errorf("a check saw the runner's credentials (exit %d)", results[0].ExitCode)
	}
}
