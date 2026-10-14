package web_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/api"
	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/web"
)

// TestDashboardSmoke drives scripts/ui-smoke.mjs — the simplified navigation and the admin
// area in a real browser — against a throwaway control plane. It needs Postgres, Node and
// Playwright with a browser it can launch, and skips when any is missing, which is the case
// in CI.
func TestDashboardSmoke(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	nodePath := os.Getenv("NODE_PATH")
	if nodePath == "" {
		if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
			nodePath = string(trimNewline(out))
		}
	}
	probe := exec.Command(node, "-e", `const fs=require("fs");const p=require("playwright").chromium.executablePath();process.exit(fs.existsSync(p)?0:1)`)
	probe.Env = append(os.Environ(), "NODE_PATH="+nodePath)
	if err := probe.Run(); err != nil {
		t.Skip("playwright with a browser is not available")
	}

	ctx := context.Background()
	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	org, err := store.CreateOrganization(ctx, fmt.Sprintf("smoke-%d", suffix), "Smoke")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, db.CreateProjectParams{OrganizationID: org.ID, Slug: "app", Config: domain.DefaultProjectConfig()})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, "ada", "Ada", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, project.ID, admin.ID, domain.RoleOrgAdmin); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateToken(ctx, admin.ID, "smoke", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(store, coord.New(store), api.Options{Web: web.Handler()}).Handler())
	defer srv.Close()

	script, _ := filepath.Abs("../../scripts/ui-smoke.mjs")
	cmd := exec.Command(node, script, srv.URL, token, "app")
	cmd.Env = append(os.Environ(), "NODE_PATH="+nodePath)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("ui smoke: %v", err)
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
