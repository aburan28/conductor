package awscreds

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/backup"
)

// ---------------------------------------------------------------------------
// EC2 instance role (IMDSv2)
// ---------------------------------------------------------------------------

// IMDS reads the EC2 instance role's credentials through the instance metadata service,
// version 2 (a session token first, then the role). It reports "not configured" quickly
// when there is no metadata service, which is the case on a laptop.
func IMDS(env Env) Provider {
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		if strings.EqualFold(env.Getenv("AWS_EC2_METADATA_DISABLED"), "true") {
			return backup.Credentials{}, notConfigured("AWS_EC2_METADATA_DISABLED is set")
		}
		base := firstNonEmpty(env.IMDSEndpoint, env.Getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT"), "http://169.254.169.254")
		base = strings.TrimRight(base, "/")
		// The metadata service answers in well under a second when it exists; a laptop has
		// none, and waiting longer would only delay the error.
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil)
		req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
		resp, err := env.HTTP.Do(req)
		if err != nil {
			return backup.Credentials{}, notConfigured("no EC2 instance metadata service")
		}
		token, err := readBody(resp, http.StatusOK)
		if err != nil {
			return backup.Credentials{}, notConfigured("no EC2 instance metadata token: %v", err)
		}
		get := func(path string) ([]byte, int, error) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
			req.Header.Set("X-aws-ec2-metadata-token", string(token))
			resp, err := env.HTTP.Do(req)
			if err != nil {
				return nil, 0, err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			return body, resp.StatusCode, err
		}
		body, code, err := get("/latest/meta-data/iam/security-credentials/")
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("instance metadata: %w", err)
		}
		if code == http.StatusNotFound {
			return backup.Credentials{}, notConfigured("this instance has no IAM role")
		}
		role := strings.TrimSpace(strings.SplitN(string(body), "\n", 2)[0])
		if code != http.StatusOK || role == "" {
			return backup.Credentials{}, fmt.Errorf("instance metadata: listing roles returned %d", code)
		}
		body, code, err = get("/latest/meta-data/iam/security-credentials/" + url.PathEscape(role))
		if err != nil || code != http.StatusOK {
			return backup.Credentials{}, fmt.Errorf("instance metadata: reading role %s returned %d %v", role, code, err)
		}
		var c containerCreds
		if err := json.Unmarshal(body, &c); err != nil {
			return backup.Credentials{}, fmt.Errorf("instance metadata: role %s: %w", role, err)
		}
		if c.Code != "" && c.Code != "Success" {
			return backup.Credentials{}, fmt.Errorf("instance metadata: role %s: %s", role, c.Code)
		}
		return c.credentials("instance role " + role)
	})
}

// containerCreds is the JSON shape IMDS and the ECS agent both return.
type containerCreds struct {
	Code            string `json:"Code"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func (c containerCreds) credentials(source string) (backup.Credentials, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return backup.Credentials{}, fmt.Errorf("%s returned no keys", source)
	}
	out := backup.Credentials{AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey, SessionToken: c.Token, Source: source}
	if c.Expiration != "" {
		t, err := time.Parse(time.RFC3339, c.Expiration)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("%s: bad expiration %q", source, c.Expiration)
		}
		out.Expires = t
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// ECS container credentials
// ---------------------------------------------------------------------------

// ECS reads the task role from the ECS (or EKS Pod Identity) credentials endpoint named by
// AWS_CONTAINER_CREDENTIALS_RELATIVE_URI or AWS_CONTAINER_CREDENTIALS_FULL_URI.
func ECS(env Env) Provider {
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		var target string
		switch {
		case env.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "":
			target = strings.TrimRight(firstNonEmpty(env.ECSEndpoint, "http://169.254.170.2"), "/") +
				env.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
		case env.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "":
			target = env.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
		default:
			return backup.Credentials{}, notConfigured("no ECS container credentials")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("container credentials: %w", err)
		}
		auth := env.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
		if f := env.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); f != "" {
			b, err := env.ReadFile(f)
			if err != nil {
				return backup.Credentials{}, fmt.Errorf("container credentials: reading the authorization token: %w", err)
			}
			auth = strings.TrimSpace(string(b))
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := env.HTTP.Do(req)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("container credentials: %w", err)
		}
		body, err := readBody(resp, http.StatusOK)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("container credentials: %w", err)
		}
		var c containerCreds
		if err := json.Unmarshal(body, &c); err != nil {
			return backup.Credentials{}, fmt.Errorf("container credentials: %w", err)
		}
		return c.credentials("ECS container credentials")
	})
}

// ---------------------------------------------------------------------------
// STS
// ---------------------------------------------------------------------------

func stsEndpoint(env Env, region string) string {
	if env.STSEndpoint != "" {
		return strings.TrimRight(env.STSEndpoint, "/")
	}
	if region == "" {
		region = "us-east-1"
	}
	return "https://sts." + region + ".amazonaws.com"
}

type stsCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type stsError struct {
	Error struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// AssumeRoleInput is what STS AssumeRole needs besides the caller's own credentials.
type AssumeRoleInput struct {
	RoleARN         string
	SessionName     string
	ExternalID      string
	DurationSeconds int
	Region          string
}

// AssumeRole calls STS AssumeRole, signed with the source provider's credentials.
func AssumeRole(env Env, source Provider, in AssumeRoleInput) Provider {
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		base, err := source.Retrieve(ctx)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("assuming %s: source credentials: %w", in.RoleARN, err)
		}
		form := url.Values{
			"Action":          {"AssumeRole"},
			"Version":         {"2011-06-15"},
			"RoleArn":         {in.RoleARN},
			"RoleSessionName": {firstNonEmpty(in.SessionName, defaultSessionName(env))},
		}
		if in.ExternalID != "" {
			form.Set("ExternalId", in.ExternalID)
		}
		if in.DurationSeconds > 0 {
			form.Set("DurationSeconds", strconv.Itoa(in.DurationSeconds))
		}
		body := []byte(form.Encode())
		region := firstNonEmpty(in.Region, "us-east-1")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, stsEndpoint(env, region)+"/", strings.NewReader(string(body)))
		if err != nil {
			return backup.Credentials{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		if err := backup.SignV4(req, body, base, region, "sts", env.Now()); err != nil {
			return backup.Credentials{}, err
		}
		var out struct {
			Result struct {
				Credentials stsCredentials `xml:"Credentials"`
			} `xml:"AssumeRoleResult"`
		}
		if err := doSTS(env, req, &out); err != nil {
			return backup.Credentials{}, fmt.Errorf("assuming %s: %w", in.RoleARN, err)
		}
		return out.Result.Credentials.credentials("assumed role " + in.RoleARN)
	})
}

// AssumeRoleWithWebIdentity exchanges an OIDC token (read from tokenFile on every call, since
// the orchestrator rotates it) for role credentials. The call is unsigned by design.
func AssumeRoleWithWebIdentity(env Env, roleARN, sessionName, tokenFile, region string) Provider {
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		token, err := env.ReadFile(tokenFile)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("web identity: reading the token: %w", err)
		}
		form := url.Values{
			"Action":           {"AssumeRoleWithWebIdentity"},
			"Version":          {"2011-06-15"},
			"RoleArn":          {roleARN},
			"RoleSessionName":  {firstNonEmpty(sessionName, defaultSessionName(env))},
			"WebIdentityToken": {strings.TrimSpace(string(token))},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, stsEndpoint(env, firstNonEmpty(region, "us-east-1"))+"/",
			strings.NewReader(form.Encode()))
		if err != nil {
			return backup.Credentials{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		var out struct {
			Result struct {
				Credentials stsCredentials `xml:"Credentials"`
			} `xml:"AssumeRoleWithWebIdentityResult"`
		}
		if err := doSTS(env, req, &out); err != nil {
			return backup.Credentials{}, fmt.Errorf("web identity for %s: %w", roleARN, err)
		}
		return out.Result.Credentials.credentials("web identity " + roleARN)
	})
}

// WebIdentityFromEnv is the web identity source configured by AWS_WEB_IDENTITY_TOKEN_FILE
// and AWS_ROLE_ARN (how EKS IRSA and GitHub Actions OIDC hand out credentials).
func WebIdentityFromEnv(env Env, region string) Provider {
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		file, role := env.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), env.Getenv("AWS_ROLE_ARN")
		if file == "" || role == "" {
			return backup.Credentials{}, notConfigured("no web identity token")
		}
		region = firstNonEmpty(env.Getenv("AWS_REGION"), env.Getenv("AWS_DEFAULT_REGION"), region)
		return AssumeRoleWithWebIdentity(env, role, env.Getenv("AWS_ROLE_SESSION_NAME"), file, region).Retrieve(ctx)
	})
}

func doSTS(env Env, req *http.Request, out any) error {
	resp, err := env.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e stsError
		if xml.Unmarshal(body, &e) == nil && e.Error.Code != "" {
			return fmt.Errorf("STS %s: %s", e.Error.Code, e.Error.Message)
		}
		return fmt.Errorf("STS returned %d", resp.StatusCode)
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return fmt.Errorf("STS response: %w", err)
	}
	return nil
}

func (c stsCredentials) credentials(source string) (backup.Credentials, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return backup.Credentials{}, fmt.Errorf("%s: STS returned no keys", source)
	}
	out := backup.Credentials{AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey, SessionToken: c.SessionToken, Source: source}
	if c.Expiration != "" {
		t, err := time.Parse(time.RFC3339, c.Expiration)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("%s: bad expiration %q", source, c.Expiration)
		}
		out.Expires = t
	}
	return out, nil
}

func defaultSessionName(env Env) string {
	return "conductor-" + strconv.FormatInt(env.Now().Unix(), 10)
}

// ---------------------------------------------------------------------------
// IAM Identity Center (SSO)
// ---------------------------------------------------------------------------

// SSORoleCredentials exchanges a cached SSO access token for the account role's credentials
// through the SSO portal's GetRoleCredentials call.
func SSORoleCredentials(ctx context.Context, env Env, token, region, accountID, roleName, source string) (backup.Credentials, error) {
	base := env.SSOEndpoint
	if base == "" {
		base = "https://portal.sso." + region + ".amazonaws.com"
	}
	q := url.Values{"account_id": {accountID}, "role_name": {roleName}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/federation/credentials?"+q.Encode(), nil)
	if err != nil {
		return backup.Credentials{}, err
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	resp, err := env.HTTP.Do(req)
	if err != nil {
		return backup.Credentials{}, fmt.Errorf("SSO portal: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return backup.Credentials{}, ErrSSOExpired
	case resp.StatusCode != http.StatusOK:
		return backup.Credentials{}, fmt.Errorf("SSO portal returned %d", resp.StatusCode)
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"` // milliseconds since the epoch
		} `json:"roleCredentials"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return backup.Credentials{}, fmt.Errorf("SSO portal: %w", err)
	}
	rc := out.RoleCredentials
	if rc.AccessKeyID == "" || rc.SecretAccessKey == "" {
		return backup.Credentials{}, errors.New("SSO portal returned no keys")
	}
	creds := backup.Credentials{AccessKey: rc.AccessKeyID, SecretKey: rc.SecretAccessKey, SessionToken: rc.SessionToken, Source: source}
	if rc.Expiration > 0 {
		creds.Expires = time.UnixMilli(rc.Expiration)
	}
	return creds, nil
}

// ErrSSOExpired means the cached SSO session is missing or expired: `aws sso login` again.
var ErrSSOExpired = errors.New("the SSO session has expired or was never started")

// ---------------------------------------------------------------------------

func readBody(resp *http.Response, want int) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != want {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
