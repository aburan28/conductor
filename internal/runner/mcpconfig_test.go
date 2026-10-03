package runner

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/harness"
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
		MCPToken:          "t",
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

	path, err := r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "cairn")
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
	path, err = r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "claude")
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

	path, err := r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "cairn")
	if err != nil {
		t.Fatal(err)
	}
	if got := readServers(t, path)["conductor"]["command"]; got != "conductor-mcp" {
		t.Errorf("conductor command = %v, want the runner's own", got)
	}
}

func TestWriteMCPConfigSkippedWithoutEndpoint(t *testing.T) {
	r := &Runner{opts: Options{}}
	path, err := r.writeMCPConfig(t.TempDir(), domain.Project{ID: "p"}, domain.Fence{}, "cairn")
	if err != nil || path != "" {
		t.Errorf("path = %q, err = %v; want no config written", path, err)
	}
}
