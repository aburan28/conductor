// Command wireguard-e2e exercises the native API from real WireGuard workers.
// It is a test fixture, not a product command. Credentials and response bodies are
// deliberately excluded from diagnostics, including failures.
package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/privacy"
)

const timeout = 5 * time.Second

type fixture struct {
	api                   *client.Client
	project, side, member string
	statePath, outputPath string
	address               string
}

type snapshot struct {
	Task         privacy.TaskView          `json:"task"`
	Lease        domain.Lease              `json:"lease"`
	Fence        domain.Fence              `json:"fence"`
	Reservations []domain.ScopeReservation `json:"reservations"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("command required")
	}
	mode := args[0]
	fs := flag.NewFlagSet("wireguard-e2e", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tokenFile := fs.String("token-file", "", "private file containing the bearer token")
	state := fs.String("state", "", "claim snapshot file")
	output := fs.String("output", "", "new private file for the invited member's token")
	member := fs.String("member", "bob", "fixture member handle")
	address := fs.String("address", "", "underlay TCP address for refusal assertion")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New("invalid command flags")
	}
	endpoint, err := url.Parse(os.Getenv("CONDUCTOR_ENDPOINT"))
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("CONDUCTOR_ENDPOINT must be an HTTPS origin")
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return errors.New("CONDUCTOR_ENDPOINT must be an HTTPS origin")
	}
	// client.New warns and falls back when a CA file is malformed. Fail closed in
	// this fixture so a broken certificate setup cannot become an ambiguous test.
	ca, err := os.ReadFile(os.Getenv("CONDUCTOR_CA_CERT"))
	if err != nil || !x509.NewCertPool().AppendCertsFromPEM(ca) {
		return errors.New("CA fixture unavailable or invalid")
	}
	var token string
	if mode != "health" && mode != "missing-token" && mode != "ca-validation" && mode != "underlay-denied" {
		b, err := privateRead(*tokenFile)
		if err != nil {
			return errors.New("private token file unavailable")
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return errors.New("private token file empty")
		}
	}
	f := fixture{api: client.New(endpoint.String(), token), project: os.Getenv("CONDUCTOR_PROJECT"), side: os.Getenv("CONDUCTOR_SIDE_PROJECT"), member: *member, statePath: *state, outputPath: *output, address: *address}
	f.api.HTTP.Timeout = timeout
	f.api.HTTP.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	transport, ok := f.api.HTTP.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil || transport.TLSClientConfig.InsecureSkipVerify {
		return errors.New("verified native-client TLS transport required")
	}
	if mode != "health" && mode != "ca-validation" && mode != "underlay-denied" && (f.project == "" || f.side == "" || f.project == f.side || f.member == "") {
		return errors.New("distinct main/side projects and member are required")
	}
	fmt.Printf("PASS native-client-api-origin %s\n", f.api.Endpoint)
	return f.execute(mode)
}

// request intentionally never includes response bodies, tokens, or transport
// errors in a diagnostic. Status/code are enough to distinguish fixture failures.
func (f *fixture) request(label, method, path, token string, body any, status int, code string, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: encode failed", label)
		}
		reader = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, f.api.Endpoint+path, reader)
	if err != nil {
		return fmt.Errorf("%s: request construction failed", label)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.api.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: request failed", label)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: response read failed", label)
	}
	if resp.StatusCode != status {
		return fmt.Errorf("%s: status=%d want=%d", label, resp.StatusCode, status)
	}
	if code != "" {
		var failure struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(b, &failure) != nil || failure.Code != code {
			return fmt.Errorf("%s: status=%d unexpected error code", label, status)
		}
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return fmt.Errorf("%s: status=%d decode failed", label, status)
	}
	fmt.Printf("PASS %s status=%d\n", label, status)
	return nil
}

func (f *fixture) own(label, method, path string, body any, status int, code string, out any) error {
	return f.request(label, method, path, f.api.Token, body, status, code, out)
}

func (f *fixture) tasks(project string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/tasks"
}
func (f *fixture) members(project string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/members"
}
func claimBody() any {
	return map[string]any{"harness": "cli", "role": "implementer", "scopes": []domain.ScopeRequest{{Resource: "path:overlay/shared.go", Mode: domain.ModeProtectedExclusive}}}
}

func (f *fixture) loadState() (snapshot, error) {
	var s snapshot
	b, err := privateRead(f.statePath)
	if err != nil || json.Unmarshal(b, &s) != nil || s.Task.ID == "" || s.Lease.ID == "" || s.Fence.LeaseID != s.Lease.ID || s.Task.Owner == "" || len(s.Reservations) != 1 {
		return s, errors.New("claim snapshot unavailable or invalid")
	}
	return s, nil
}

func (f *fixture) execute(mode string) error {
	switch mode {
	case "underlay-denied":
		host, _, err := net.SplitHostPort(f.address)
		if err != nil || net.ParseIP(host) == nil {
			return errors.New("underlay-denied: IP:port address required")
		}
		conn, err := net.DialTimeout("tcp", f.address, timeout)
		if err == nil {
			conn.Close()
			return errors.New("underlay-denied: TCP unexpectedly connected")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return errors.New("underlay-denied: expected explicit TCP refusal")
		}
		fmt.Println("PASS underlay-application-unavailable ECONNREFUSED")
		return nil
	case "ca-validation":
		if err := f.request("trusted-ca-health", "GET", "/v1/health", "", nil, 200, "", nil); err != nil {
			return err
		}
		// Retain the native transport's TLS settings, replacing only trust roots.
		// An empty pool must reject this same reachable TLS service explicitly
		// for certificate authority validation, not a timeout or HTTP rejection.
		transport := f.api.HTTP.Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.RootCAs = x509.NewCertPool()
		defer transport.CloseIdleConnections()
		probe := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: f.api.HTTP.CheckRedirect}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", f.api.Endpoint+"/v1/health", nil)
		if err != nil {
			return errors.New("untrusted-ca: request construction failed")
		}
		resp, err := probe.Do(req)
		if err == nil {
			resp.Body.Close()
			return errors.New("untrusted-ca: TLS unexpectedly accepted")
		}
		var authority x509.UnknownAuthorityError
		if !errors.As(err, &authority) {
			return errors.New("untrusted-ca: expected certificate authority refusal")
		}
		fmt.Println("PASS untrusted-ca-refused TLS-unknown-authority")
		return nil
	case "health":
		var health struct {
			Status string `json:"status"`
		}
		if err := f.request("health", "GET", "/v1/health", "", nil, 200, "", &health); err != nil {
			return err
		}
		if health.Status != "ok" {
			return errors.New("health: unexpected state")
		}
		return nil
	case "setup":
		var invite struct {
			Token   string `json:"token"`
			Created bool   `json:"created_principal"`
		}
		if err := f.own("invite-member", "POST", f.members(f.project), map[string]any{"handle": f.member, "role": "contributor"}, 201, "", &invite); err != nil {
			return err
		}
		if !invite.Created || invite.Token == "" {
			return errors.New("invite-member: new credential missing")
		}
		return privateWrite(f.outputPath, []byte(invite.Token+"\n"))
	case "claim":
		var identity struct {
			Principal domain.Principal `json:"principal"`
		}
		if err := f.own("claimant-identity", "GET", "/v1/whoami", nil, 200, "", &identity); err != nil {
			return err
		}
		if identity.Principal.ID == "" || identity.Principal.Handle == "" {
			return errors.New("claimant-identity: identity missing")
		}
		var task privacy.TaskView
		if err := f.own("create-task", "POST", f.tasks(f.project), map[string]any{"title": "WireGuard native authorization fixture", "status": "ready", "visibility": "team_artifacts"}, 201, "", &task); err != nil {
			return err
		}
		if task.ID == "" {
			return errors.New("create-task: task identifier missing")
		}
		var s snapshot
		if err := f.own("claim-task", "POST", "/v1/tasks/"+string(task.ID)+"/claim", claimBody(), 200, "", &s); err != nil {
			return err
		}
		if s.Task.ID != task.ID || s.Task.Status != domain.TaskClaimed || s.Task.Owner != identity.Principal.Handle || s.Lease.HolderPrincipal != identity.Principal.ID || s.Fence.FencingEpoch < 1 || s.Task.FencingEpoch != s.Fence.FencingEpoch || s.Lease.FencingEpoch != s.Fence.FencingEpoch || s.Fence.TaskID != task.ID || s.Lease.TaskID != task.ID || s.Fence.LeaseID == "" || s.Fence.LeaseID != s.Lease.ID || s.Fence.AttemptID == "" || s.Fence.AttemptID != s.Lease.AttemptID || !s.Lease.Active(time.Now()) || len(s.Reservations) != 1 {
			return errors.New("claim-task: inconsistent claim snapshot")
		}
		r := s.Reservations[0]
		if !r.Active || r.LeaseID != s.Lease.ID || r.AttemptID != s.Fence.AttemptID || r.PrincipalID != s.Lease.HolderPrincipal || r.Resource() != "path:overlay/shared.go" || r.Mode != domain.ModeProtectedExclusive {
			return errors.New("claim-task: reservation mismatch")
		}
		b, err := json.Marshal(s)
		if err != nil {
			return errors.New("claim-task: snapshot encoding failed")
		}
		return privateWrite(f.statePath, b)
	case "conflict":
		s, err := f.loadState()
		if err != nil {
			return err
		}
		if err := f.own("conflicting-claim", "POST", "/v1/tasks/"+string(s.Task.ID)+"/claim", claimBody(), 409, "already_claimed", nil); err != nil {
			return err
		}
		return f.own("contributor-cannot-invite", "POST", f.members(f.project), map[string]any{"handle": "forbidden-overlay-member", "role": "project_admin"}, 403, "forbidden", nil)
	case "outsider", "after-remove-main", "missing-token":
		s, err := f.loadState()
		if err != nil {
			return err
		}
		status, code := 404, "not_found"
		if mode == "missing-token" {
			status, code = 401, "unauthenticated"
		} else {
			if err := f.own(mode+"-whoami", "GET", "/v1/whoami", nil, 200, "", nil); err != nil {
				return err
			}
			if err := f.own(mode+"-side-project", "GET", f.tasks(f.side), nil, 200, "", nil); err != nil {
				return err
			}
		}
		if err := f.own(mode+"-main-read", "GET", f.tasks(f.project), nil, status, code, nil); err != nil {
			return err
		}
		return f.own(mode+"-main-claim", "POST", "/v1/tasks/"+string(s.Task.ID)+"/claim", claimBody(), status, code, nil)
	case "token-revocation":
		const name = "overlay-revocable"
		var minted struct {
			Token string `json:"token"`
		}
		if err := f.own("create-disposable-token", "POST", "/v1/tokens", map[string]any{"name": name, "ttl": "1h"}, 201, "", &minted); err != nil {
			return err
		}
		if minted.Token == "" {
			return errors.New("create-disposable-token: credential missing")
		}
		if err := f.request("disposable-token-works", "GET", f.tasks(f.project), minted.Token, nil, 200, "", nil); err != nil {
			return err
		}
		if err := f.own("revoke-disposable-token", "DELETE", "/v1/tokens/"+name, nil, 204, "", nil); err != nil {
			return err
		}
		if err := f.request("revoked-token-refused", "GET", f.tasks(f.project), minted.Token, nil, 401, "unauthenticated", nil); err != nil {
			return err
		}
		return f.own("original-token-still-works", "GET", f.tasks(f.project), nil, 200, "", nil)
	case "grant-side":
		var invite struct {
			Token    string `json:"token"`
			Existing bool   `json:"existing_principal"`
		}
		if err := f.own("grant-side-membership", "POST", f.members(f.side), map[string]any{"handle": f.member, "role": "contributor"}, 201, "", &invite); err != nil {
			return err
		}
		if invite.Token != "" || !invite.Existing {
			return errors.New("grant-side-membership: unexpected credential or principal")
		}
		return nil
	case "remove-main":
		return f.own("remove-main-membership", "DELETE", f.members(f.project)+"/"+url.PathEscape(f.member), nil, 204, "", nil)
	case "remove-side":
		return f.own("remove-final-membership", "DELETE", f.members(f.side)+"/"+url.PathEscape(f.member), nil, 204, "", nil)
	case "after-remove-all":
		if err := f.own("final-removal-invalidates-token", "GET", "/v1/whoami", nil, 401, "unauthenticated", nil); err != nil {
			return err
		}
		return f.own("final-removal-refuses-project", "GET", f.tasks(f.side), nil, 401, "unauthenticated", nil)
	case "verify-owner":
		s, err := f.loadState()
		if err != nil {
			return err
		}
		var task privacy.TaskView
		if err := f.own("owner-task-read", "GET", "/v1/tasks/"+string(s.Task.ID), nil, 200, "", &task); err != nil {
			return err
		}
		if task.ID != s.Task.ID || task.Owner != s.Task.Owner || task.Status != s.Task.Status || task.FencingEpoch != s.Fence.FencingEpoch || task.AttemptsCount != s.Task.AttemptsCount {
			return errors.New("verify-owner: task authority changed")
		}
		var out struct {
			Reservations []domain.ScopeReservation `json:"reservations"`
		}
		if err := f.own("owner-reservations-read", "GET", "/v1/projects/"+url.PathEscape(f.project)+"/reservations", nil, 200, "", &out); err != nil {
			return err
		}
		found := 0
		for _, r := range out.Reservations {
			if r.TaskID != s.Task.ID || !r.Active {
				continue
			}
			if r.ID != s.Reservations[0].ID || r.LeaseID != s.Lease.ID || r.AttemptID != s.Fence.AttemptID || r.PrincipalID != s.Lease.HolderPrincipal || r.Mode != domain.ModeProtectedExclusive || r.Resource() != "path:overlay/shared.go" {
				return errors.New("verify-owner: reservation authority changed")
			}
			found++
		}
		if found != 1 || !s.Lease.Active(time.Now()) {
			return errors.New("verify-owner: original live reservation missing or expired")
		}
		fmt.Println("PASS owner-and-fence-unchanged")
		return nil
	default:
		return errors.New("unknown command")
	}
}

func privateRead(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1<<20 {
		return nil, errors.New("private file required")
	}
	return os.ReadFile(path)
}

func privateWrite(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("private output creation failed")
	}
	_, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("private output write failed")
	}
	return nil
}
