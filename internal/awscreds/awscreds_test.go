package awscreds

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/backup"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// testEnv is an environment with a temporary home and no network unless a test adds it.
func testEnv(t *testing.T, vars map[string]string) (Env, string) {
	t.Helper()
	home := t.TempDir()
	env := Env{
		Getenv:   func(k string) string { return vars[k] },
		ReadFile: os.ReadFile,
		HomeDir:  func() (string, error) { return home, nil },
		Now:      func() time.Time { return testNow },
		HTTP:     &http.Client{Timeout: 5 * time.Second},
		Command: func(context.Context, []byte, string, ...string) ([]byte, error) {
			return nil, errors.New("no command expected")
		},
		IMDSEndpoint: "http://127.0.0.1:1", // nothing listens: "no metadata service"
		GOOS:         "linux",
	}
	return env, home
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func retrieve(t *testing.T, p Provider) backup.Credentials {
	t.Helper()
	c, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	return c
}

func TestParseINI(t *testing.T) {
	f := parseINI([]byte(`
# comment
[default]
region = us-west-2
s3 =
  max_concurrent_requests = 20
; another comment
[profile  dev ]
role_arn = arn:aws:iam::1:role/x
`))
	if f["default"]["region"] != "us-west-2" {
		t.Errorf("default region: %v", f["default"])
	}
	if _, ok := f["default"]["max_concurrent_requests"]; ok {
		t.Error("a nested sub-setting leaked into the section")
	}
	if f["profile dev"]["role_arn"] != "arn:aws:iam::1:role/x" {
		t.Errorf("profile section name not normalised: %v", f)
	}
}

func TestStaticProfileAndListing(t *testing.T) {
	env, home := testEnv(t, nil)
	writeFile(t, filepath.Join(home, ".aws", "credentials"), "[work]\naws_access_key_id = AKIDWORK\naws_secret_access_key = s3cr3t\n")
	writeFile(t, filepath.Join(home, ".aws", "config"), `[profile work]
region = eu-west-1
[profile sso-dev]
sso_session = corp
sso_account_id = 111
sso_role_name = Dev
[sso-session corp]
sso_start_url = https://corp.awsapps.com/start
sso_region = us-east-1
[profile admin]
role_arn = arn:aws:iam::222:role/Admin
source_profile = work
`)
	c := retrieve(t, Profile(env, "work"))
	if c.AccessKey != "AKIDWORK" || c.SecretKey != "s3cr3t" || c.Source != "profile work" {
		t.Fatalf("static profile: %+v", c)
	}
	if r := ProfileRegion(env, "work"); r != "eu-west-1" {
		t.Errorf("region: %q", r)
	}
	list, err := Profiles(env)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, p := range list {
		kinds[p.Name] = p.Kind
	}
	want := map[string]string{"work": "static", "sso-dev": "sso", "admin": "assume-role"}
	for name, kind := range want {
		if kinds[name] != kind {
			t.Errorf("profile %s kind = %q, want %q (all: %v)", name, kinds[name], kind, kinds)
		}
	}
	if _, err := Profile(env, "nope").Retrieve(context.Background()); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("missing profile error: %v", err)
	}
}

func TestAssumeRoleSignsWithSourceProfile(t *testing.T) {
	var gotForm url.Values
	var gotAuth string
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials>
<AccessKeyId>ASIAROLE</AccessKeyId><SecretAccessKey>rolesecret</SecretAccessKey>
<SessionToken>tok</SessionToken><Expiration>2026-10-07T13:00:00Z</Expiration>
</Credentials></AssumeRoleResult></AssumeRoleResponse>`)
	}))
	defer sts.Close()

	env, home := testEnv(t, nil)
	env.STSEndpoint = sts.URL
	writeFile(t, filepath.Join(home, ".aws", "credentials"), "[base]\naws_access_key_id = AKIDBASE\naws_secret_access_key = basesecret\n")
	writeFile(t, filepath.Join(home, ".aws", "config"),
		"[profile admin]\nrole_arn = arn:aws:iam::222:role/Admin\nsource_profile = base\nexternal_id = ext-1\nregion = eu-central-1\n")

	c := retrieve(t, Profile(env, "admin"))
	if c.AccessKey != "ASIAROLE" || c.SessionToken != "tok" || !c.Expires.Equal(time.Date(2026, 10, 7, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("assumed creds: %+v", c)
	}
	if gotForm.Get("Action") != "AssumeRole" || gotForm.Get("RoleArn") != "arn:aws:iam::222:role/Admin" || gotForm.Get("ExternalId") != "ext-1" {
		t.Errorf("form: %v", gotForm)
	}
	if !strings.Contains(gotAuth, "Credential=AKIDBASE/20261007/eu-central-1/sts/aws4_request") {
		t.Errorf("STS call not signed with the source profile for sts in the profile's region: %q", gotAuth)
	}
}

func TestSSOProfile(t *testing.T) {
	var gotToken, gotQuery string
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotQuery = r.Header.Get("x-amz-sso_bearer_token"), r.URL.RawQuery
		if gotToken != "good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"roleCredentials":{"accessKeyId":"ASIASSO","secretAccessKey":"ssosecret","sessionToken":"ssotok","expiration":%d}}`,
			testNow.Add(time.Hour).UnixMilli())
	}))
	defer portal.Close()

	env, home := testEnv(t, nil)
	env.SSOEndpoint = portal.URL
	writeFile(t, filepath.Join(home, ".aws", "config"), `[profile dev]
sso_session = corp
sso_account_id = 111122223333
sso_role_name = Developer
[sso-session corp]
sso_start_url = https://corp.awsapps.com/start
sso_region = us-east-1
`)
	sum := sha1.Sum([]byte("corp"))
	cache := filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json")

	// No cache yet: tell the user to log in.
	_, err := Profile(env, "dev").Retrieve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "aws sso login --profile dev") {
		t.Fatalf("missing token should say to log in: %v", err)
	}

	writeFile(t, cache, `{"accessToken":"good-token","expiresAt":"2026-10-07T20:00:00Z"}`)
	c := retrieve(t, Profile(env, "dev"))
	if c.AccessKey != "ASIASSO" || c.SessionToken != "ssotok" || c.Source != "profile dev (SSO)" {
		t.Fatalf("sso creds: %+v", c)
	}
	if !strings.Contains(gotQuery, "account_id=111122223333") || !strings.Contains(gotQuery, "role_name=Developer") {
		t.Errorf("portal query: %s", gotQuery)
	}

	// An expired token in the cache is not sent.
	writeFile(t, cache, `{"accessToken":"good-token","expiresAt":"2026-10-07T11:00:00Z"}`)
	if _, err := Profile(env, "dev").Retrieve(context.Background()); err == nil || !errors.Is(err, ErrSSOExpired) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestCredentialProcess(t *testing.T) {
	env, home := testEnv(t, nil)
	var gotArgv []string
	env.Command = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		gotArgv = append([]string{name}, args...)
		return []byte(`{"Version":1,"AccessKeyId":"AKIDPROC","SecretAccessKey":"procsecret","Expiration":"2026-10-07T12:30:00Z"}`), nil
	}
	writeFile(t, filepath.Join(home, ".aws", "config"), "[default]\ncredential_process = /opt/bin/vault-creds --role \"read only\" 'x y'\n")
	c := retrieve(t, Profile(env, ""))
	if c.AccessKey != "AKIDPROC" || c.Expires.IsZero() {
		t.Fatalf("process creds: %+v", c)
	}
	want := []string{"/opt/bin/vault-creds", "--role", "read only", "x y"}
	if strings.Join(gotArgv, "|") != strings.Join(want, "|") {
		t.Errorf("argv = %q, want %q", gotArgv, want)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`a b  c`:            {"a", "b", "c"},
		`a "b c" d`:         {"a", "b c", "d"},
		`a 'b "c"'`:         {"a", `b "c"`},
		`a b\ c`:            {"a", "b c"},
		`"" x`:              {"", "x"},
		`/p/x --k="v w" -z`: {"/p/x", "--k=v w", "-z"},
	}
	for in, want := range cases {
		got, err := splitCommand(in)
		if err != nil || strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("splitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitCommand(`a "b`); err == nil {
		t.Error("unterminated quote accepted")
	}
}

func TestEnvironmentChain(t *testing.T) {
	// Variables win.
	env, _ := testEnv(t, map[string]string{"AWS_ACCESS_KEY_ID": "AKIDENV", "AWS_SECRET_ACCESS_KEY": "envsecret", "AWS_SESSION_TOKEN": "t"})
	c := retrieve(t, Environment(env, "us-east-1"))
	if c.AccessKey != "AKIDENV" || c.SessionToken != "t" {
		t.Fatalf("env creds: %+v", c)
	}

	// Nothing at all: one clear error, quickly (IMDS points at a closed port).
	env, _ = testEnv(t, nil)
	start := time.Now()
	_, err := Environment(env, "us-east-1").Retrieve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no AWS credentials found") {
		t.Fatalf("empty environment: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("an absent metadata service took %v to rule out", time.Since(start))
	}
}

func TestECSCredentials(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/credentials/abc" || r.Header.Get("Authorization") != "ecs-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"AccessKeyId":"ASIAECS","SecretAccessKey":"ecssecret","Token":"ecstok","Expiration":"2026-10-07T18:00:00Z"}`)
	}))
	defer agent.Close()
	env, home := testEnv(t, nil)
	tokenFile := filepath.Join(home, "tok")
	writeFile(t, tokenFile, "ecs-token\n")
	env.Getenv = func(k string) string {
		return map[string]string{
			"AWS_CONTAINER_CREDENTIALS_FULL_URI":     agent.URL + "/v2/credentials/abc",
			"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": tokenFile,
		}[k]
	}
	c := retrieve(t, Environment(env, "us-east-1"))
	if c.AccessKey != "ASIAECS" || c.Source != "ECS container credentials" {
		t.Fatalf("ecs creds: %+v", c)
	}
}

func TestIMDSv2(t *testing.T) {
	var tokenRequests atomic.Int32
	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tokenRequests.Add(1)
			fmt.Fprint(w, "imds-session")
		case r.Header.Get("X-aws-ec2-metadata-token") != "imds-session":
			w.WriteHeader(http.StatusUnauthorized) // IMDSv1 is refused, as on a hardened instance
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			fmt.Fprint(w, "app-role\n")
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/app-role":
			fmt.Fprint(w, `{"Code":"Success","AccessKeyId":"ASIAEC2","SecretAccessKey":"ec2secret","Token":"ec2tok","Expiration":"2026-10-07T18:00:00Z"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer imds.Close()
	env, _ := testEnv(t, nil)
	env.IMDSEndpoint = imds.URL
	c := retrieve(t, Environment(env, "us-east-1"))
	if c.AccessKey != "ASIAEC2" || c.Source != "instance role app-role" || tokenRequests.Load() != 1 {
		t.Fatalf("imds creds: %+v (token requests %d)", c, tokenRequests.Load())
	}
}

func TestWebIdentity(t *testing.T) {
	var gotForm url.Values
	var signed bool
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		signed = r.Header.Get("Authorization") != ""
		fmt.Fprint(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
<AccessKeyId>ASIAWEB</AccessKeyId><SecretAccessKey>websecret</SecretAccessKey><SessionToken>webtok</SessionToken>
<Expiration>2026-10-07T13:00:00Z</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`)
	}))
	defer sts.Close()
	env, home := testEnv(t, nil)
	tokenFile := filepath.Join(home, "oidc")
	writeFile(t, tokenFile, "eyJ.oidc.token\n")
	env.STSEndpoint = sts.URL
	env.Getenv = func(k string) string {
		return map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tokenFile, "AWS_ROLE_ARN": "arn:aws:iam::3:role/ci"}[k]
	}
	c := retrieve(t, Environment(env, "us-east-1"))
	if c.AccessKey != "ASIAWEB" {
		t.Fatalf("web identity creds: %+v", c)
	}
	if gotForm.Get("WebIdentityToken") != "eyJ.oidc.token" || gotForm.Get("RoleArn") != "arn:aws:iam::3:role/ci" || signed {
		t.Errorf("form %v, signed %v", gotForm, signed)
	}
}

func TestSTSErrorIsReported(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>not allowed</Message></Error></ErrorResponse>`)
	}))
	defer sts.Close()
	env, _ := testEnv(t, nil)
	env.STSEndpoint = sts.URL
	src := Static{Creds: backup.Credentials{AccessKey: "A", SecretKey: "S"}}
	_, err := AssumeRole(env, src, AssumeRoleInput{RoleARN: "arn:x"}).Retrieve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AccessDenied: not allowed") {
		t.Fatalf("error: %v", err)
	}
}

func TestCachedRefreshesBeforeExpiry(t *testing.T) {
	now := testNow
	var calls int
	p := NewCached(ProviderFunc(func(context.Context) (backup.Credentials, error) {
		calls++
		return backup.Credentials{AccessKey: fmt.Sprint("K", calls), SecretKey: "S", Expires: now.Add(time.Hour)}, nil
	}), func() time.Time { return now })
	first := retrieve(t, p)
	_ = retrieve(t, p)
	if calls != 1 {
		t.Fatalf("cached credentials were fetched %d times", calls)
	}
	now = now.Add(56 * time.Minute) // inside the refresh window
	second := retrieve(t, p)
	if calls != 2 || second.AccessKey == first.AccessKey {
		t.Fatalf("credentials near expiry were not refreshed (calls %d)", calls)
	}
}

func TestKeychain(t *testing.T) {
	env, _ := testEnv(t, nil)
	if _, err := KeychainGet(context.Background(), env, KeychainS3Service, "AKID"); !errors.Is(err, ErrNoKeychain) {
		t.Fatalf("off macOS: %v", err)
	}
	env.GOOS = "darwin"
	store := map[string]string{}
	var argvSeen []string
	env.Command = func(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		argvSeen = append(argvSeen, strings.Join(append([]string{name}, args...), " "))
		if name != securityTool {
			return nil, fmt.Errorf("unexpected %s", name)
		}
		switch args[0] {
		case "-i":
			// add-generic-password -U -s "svc" -a "acct" -w "secret"
			fields, err := splitCommand(strings.TrimSpace(string(stdin)))
			if err != nil || fields[0] != "add-generic-password" {
				return nil, fmt.Errorf("bad interactive line %q", stdin)
			}
			store[fields[3]+"/"+fields[5]] = fields[7]
			return nil, nil
		case "find-generic-password":
			v, ok := store[args[2]+"/"+args[4]]
			if !ok {
				return nil, errors.New("security: exit status 44")
			}
			return []byte(v + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected args %v", args)
	}
	ctx := context.Background()
	if _, err := KeychainGet(ctx, env, KeychainS3Service, "AKID"); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Fatalf("missing item: %v", err)
	}
	secret := `wJal"rXUt\nFEMI/K7MDENG+bPxRfiCY`
	if err := KeychainSet(ctx, env, KeychainS3Service, "AKID", secret); err != nil {
		t.Fatal(err)
	}
	for _, argv := range argvSeen {
		if strings.Contains(argv, "rXUt") {
			t.Fatalf("the secret appeared on a command line: %s", argv)
		}
	}
	c := retrieve(t, StaticKeychain(env, "AKID"))
	if c.SecretKey != secret || c.AccessKey != "AKID" {
		t.Fatalf("round trip: %+v", c)
	}
}
