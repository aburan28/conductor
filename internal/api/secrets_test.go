package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/secretbox"
	"github.com/aburan28/conductor/internal/secrets"
)

func (h *harness) withSecrets(t *testing.T) *secrets.Vault {
	t.Helper()
	raw, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	svc := coord.New(h.store)
	v := secrets.New(h.store, &secretbox.Source{Env: raw})
	h.server.Close()
	h.server = httptest.NewServer(New(h.store, svc, Options{Secrets: v, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	t.Cleanup(h.server.Close)
	t.Cleanup(func() {
		_, _ = h.store.Pool().Exec(context.Background(),
			`DELETE FROM project_secrets WHERE project_id = $1::uuid`, h.project.ID)
	})
	return v
}

func TestProjectSecretsRolesAndNoListLeak(t *testing.T) {
	h := newHarness(t)
	h.withSecrets(t)
	_, obsTok := h.member("olive", domain.RoleObserver)
	_, maintTok := h.member("mia", domain.RoleMaintainer)

	body := map[string]any{"name": "DATABASE_URL", "value": "postgres://secret"}
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/secrets"), body); code != http.StatusForbidden {
		t.Fatalf("contributor POST = %d", code)
	}
	if code, _ := h.do(obsTok, http.MethodPost, h.projectPath("/secrets"), body); code != http.StatusForbidden {
		t.Fatalf("observer POST = %d", code)
	}
	code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/secrets"), body)
	if code != http.StatusCreated {
		t.Fatalf("maintainer POST = %d\n%s", code, resp)
	}
	if strings.Contains(string(resp), "postgres://secret") {
		t.Fatalf("create response leaked plaintext: %s", resp)
	}

	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/secrets"), nil)
	if code != http.StatusOK {
		t.Fatalf("contributor list = %d", code)
	}
	if strings.Contains(string(resp), "postgres://secret") {
		t.Fatalf("list leaked plaintext: %s", resp)
	}

	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/secrets/DATABASE_URL"), nil)
	if code != http.StatusOK {
		t.Fatalf("contributor get = %d\n%s", code, resp)
	}
	var opened secrets.Opened
	_ = json.Unmarshal(resp, &opened)
	if opened.Value != "postgres://secret" {
		t.Fatalf("opened = %+v", opened)
	}

	if code, _ := h.do(obsTok, http.MethodGet, h.projectPath("/secrets/DATABASE_URL"), nil); code != http.StatusForbidden {
		t.Fatalf("observer get = %d", code)
	}
}
