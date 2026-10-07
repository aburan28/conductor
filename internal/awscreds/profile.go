package awscreds

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/backup"
)

// iniFile is a parsed AWS config or credentials file: section name → key → value. Keys are
// lowercased. Nested sub-settings (indented lines under a key such as `s3 =`) are skipped;
// none of them bear on credentials.
type iniFile map[string]map[string]string

func parseINI(data []byte) iniFile {
	out := iniFile{}
	var section map[string]string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")), " ")
			section = out[name]
			if section == nil {
				section = map[string]string{}
				out[name] = section
			}
			continue
		}
		if section == nil || raw[0] == ' ' || raw[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		section[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

// awsFiles are the user's parsed ~/.aws/config and ~/.aws/credentials.
type awsFiles struct {
	config      iniFile
	credentials iniFile
}

func loadAWSFiles(env Env) (awsFiles, error) {
	home, _ := env.HomeDir()
	cfgPath := firstNonEmpty(env.Getenv("AWS_CONFIG_FILE"), filepath.Join(home, ".aws", "config"))
	credPath := firstNonEmpty(env.Getenv("AWS_SHARED_CREDENTIALS_FILE"), filepath.Join(home, ".aws", "credentials"))
	var f awsFiles
	if b, err := env.ReadFile(cfgPath); err == nil {
		f.config = parseINI(b)
	}
	if b, err := env.ReadFile(credPath); err == nil {
		f.credentials = parseINI(b)
	}
	if f.config == nil && f.credentials == nil {
		return f, fmt.Errorf("no AWS config found (%s, %s)", cfgPath, credPath)
	}
	return f, nil
}

// profile merges a profile's settings: ~/.aws/config's [profile NAME] (or [default]),
// overlaid by ~/.aws/credentials' [NAME], which is where the CLI keeps keys.
func (f awsFiles) profile(name string) (map[string]string, bool) {
	merged := map[string]string{}
	found := false
	section := "profile " + name
	if name == "default" {
		if s, ok := f.config["default"]; ok {
			for k, v := range s {
				merged[k] = v
			}
			found = true
		}
	}
	if s, ok := f.config[section]; ok {
		for k, v := range s {
			merged[k] = v
		}
		found = true
	}
	if s, ok := f.credentials[name]; ok {
		for k, v := range s {
			merged[k] = v
		}
		found = true
	}
	return merged, found
}

// ProfileInfo describes a profile for a picker.
type ProfileInfo struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // static, sso, assume-role, process, unknown
	Region string `json:"region,omitempty"`
}

// Profiles lists every profile in the user's AWS files.
func Profiles(env Env) ([]ProfileInfo, error) {
	f, err := loadAWSFiles(env)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for s := range f.config {
		switch {
		case s == "default":
			names["default"] = true
		case strings.HasPrefix(s, "profile "):
			names[strings.TrimPrefix(s, "profile ")] = true
		}
	}
	for s := range f.credentials {
		names[s] = true
	}
	var out []ProfileInfo
	for name := range names {
		p, _ := f.profile(name)
		out = append(out, ProfileInfo{Name: name, Kind: profileKind(p), Region: p["region"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func profileKind(p map[string]string) string {
	switch {
	case p["role_arn"] != "":
		return "assume-role"
	case p["sso_session"] != "" || p["sso_start_url"] != "":
		return "sso"
	case p["credential_process"] != "":
		return "process"
	case p["aws_access_key_id"] != "":
		return "static"
	}
	return "unknown"
}

// ProfileRegion is the region a profile names, or "".
func ProfileRegion(env Env, name string) string {
	f, err := loadAWSFiles(env)
	if err != nil {
		return ""
	}
	p, _ := f.profile(profileName(env, name))
	return p["region"]
}

func profileName(env Env, name string) string {
	return firstNonEmpty(name, env.Getenv("AWS_PROFILE"), "default")
}

// Profile is the `profile` sign-in method: the named profile (empty means AWS_PROFILE, then
// default), resolved the way the AWS CLI resolves it. The result refreshes itself.
func Profile(env Env, name string) Provider {
	name = profileName(env, name)
	return NewCached(ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		f, err := loadAWSFiles(env)
		if err != nil {
			return backup.Credentials{}, err
		}
		p, err := f.resolve(env, name, 0)
		if err != nil {
			return backup.Credentials{}, err
		}
		return p.Retrieve(ctx)
	}), env.Now)
}

const maxProfileChain = 5

// resolve builds the provider for one profile. Order follows the AWS CLI: a role to assume
// first (its source being another profile, the environment, or a web identity token), then
// SSO, then credential_process, then static keys.
func (f awsFiles) resolve(env Env, name string, depth int) (Provider, error) {
	if depth > maxProfileChain {
		return nil, fmt.Errorf("profile %s: source_profile chain is longer than %d", name, maxProfileChain)
	}
	p, ok := f.profile(name)
	if !ok {
		return nil, fmt.Errorf("profile %q is not in ~/.aws/config or ~/.aws/credentials", name)
	}
	region := p["region"]

	if role := p["role_arn"]; role != "" {
		in := AssumeRoleInput{RoleARN: role, SessionName: p["role_session_name"], ExternalID: p["external_id"], Region: region}
		if d := p["duration_seconds"]; d != "" {
			n, err := strconv.Atoi(d)
			if err != nil {
				return nil, fmt.Errorf("profile %s: duration_seconds %q is not a number", name, d)
			}
			in.DurationSeconds = n
		}
		switch {
		case p["web_identity_token_file"] != "":
			return AssumeRoleWithWebIdentity(env, role, in.SessionName, p["web_identity_token_file"], region), nil
		case p["credential_source"] != "":
			var src Provider
			switch p["credential_source"] {
			case "Environment":
				src = FromEnvironment(env)
			case "Ec2InstanceMetadata":
				src = IMDS(env)
			case "EcsContainer":
				src = ECS(env)
			default:
				return nil, fmt.Errorf("profile %s: credential_source %q is not Environment, Ec2InstanceMetadata or EcsContainer",
					name, p["credential_source"])
			}
			return AssumeRole(env, src, in), nil
		case p["source_profile"] != "":
			var src Provider
			if p["source_profile"] == name {
				// A profile may name itself as its source: its own static keys assume its role.
				s, err := staticFromProfile(name, p)
				if err != nil {
					return nil, err
				}
				src = s
			} else {
				s, err := f.resolve(env, p["source_profile"], depth+1)
				if err != nil {
					return nil, err
				}
				src = s
			}
			return AssumeRole(env, src, in), nil
		default:
			return nil, fmt.Errorf("profile %s: role_arn needs source_profile, credential_source or web_identity_token_file", name)
		}
	}

	if p["sso_session"] != "" || p["sso_start_url"] != "" {
		return f.sso(env, name, p)
	}

	if cmd := p["credential_process"]; cmd != "" {
		return credentialProcess(env, name, cmd), nil
	}

	return staticFromProfile(name, p)
}

func staticFromProfile(name string, p map[string]string) (Provider, error) {
	if p["aws_access_key_id"] == "" || p["aws_secret_access_key"] == "" {
		return nil, fmt.Errorf("profile %s has no credentials (no keys, role, SSO, or credential_process)", name)
	}
	return Static{Creds: backup.Credentials{
		AccessKey: p["aws_access_key_id"], SecretKey: p["aws_secret_access_key"], SessionToken: p["aws_session_token"],
		Source: "profile " + name,
	}}, nil
}

// sso builds the IAM Identity Center source: the access token that `aws sso login` cached,
// exchanged for the account role's credentials. Conductor does not refresh the SSO token
// itself; when it expires, the error says to log in again.
func (f awsFiles) sso(env Env, name string, p map[string]string) (Provider, error) {
	account, role := p["sso_account_id"], p["sso_role_name"]
	if account == "" || role == "" {
		return nil, fmt.Errorf("profile %s: an SSO profile needs sso_account_id and sso_role_name", name)
	}
	startURL, ssoRegion, cacheKey := p["sso_start_url"], p["sso_region"], p["sso_start_url"]
	if sess := p["sso_session"]; sess != "" {
		s, ok := f.config["sso-session "+sess]
		if !ok {
			return nil, fmt.Errorf("profile %s: sso_session %q has no [sso-session %s] section", name, sess, sess)
		}
		startURL, ssoRegion, cacheKey = firstNonEmpty(s["sso_start_url"], startURL), firstNonEmpty(s["sso_region"], ssoRegion), sess
	}
	if startURL == "" || ssoRegion == "" {
		return nil, fmt.Errorf("profile %s: SSO needs sso_start_url and sso_region", name)
	}
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		token, err := cachedSSOToken(env, cacheKey)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("profile %s: %w; run `aws sso login --profile %s`", name, err, name)
		}
		creds, err := SSORoleCredentials(ctx, env, token, ssoRegion, account, role, "profile "+name+" (SSO)")
		if errors.Is(err, ErrSSOExpired) {
			return backup.Credentials{}, fmt.Errorf("profile %s: %w; run `aws sso login --profile %s`", name, err, name)
		}
		return creds, err
	}), nil
}

// cachedSSOToken reads ~/.aws/sso/cache/<sha1(key)>.json, which `aws sso login` writes.
func cachedSSOToken(env Env, key string) (string, error) {
	home, err := env.HomeDir()
	if err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(key))
	path := filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json")
	b, err := env.ReadFile(path)
	if err != nil {
		return "", ErrSSOExpired
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   string `json:"expiresAt"`
	}
	if err := json.Unmarshal(b, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("the SSO token cache %s is unreadable", path)
	}
	exp, err := parseSSOTime(tok.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("the SSO token cache %s has an unreadable expiresAt", path)
	}
	if !env.Now().Before(exp) {
		return "", ErrSSOExpired
	}
	return tok.AccessToken, nil
}

func parseSSOTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05UTC", "2006-01-02T15:04:05Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}

// credentialProcess runs a profile's credential_process and reads its JSON output.
func credentialProcess(env Env, name, command string) Provider {
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		argv, err := splitCommand(command)
		if err != nil || len(argv) == 0 {
			return backup.Credentials{}, fmt.Errorf("profile %s: credential_process %q cannot be parsed", name, command)
		}
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		out, err := env.Command(ctx, nil, argv[0], argv[1:]...)
		if err != nil {
			return backup.Credentials{}, fmt.Errorf("profile %s: credential_process failed: %w", name, err)
		}
		var c struct {
			Version         int    `json:"Version"`
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			SessionToken    string `json:"SessionToken"`
			Expiration      string `json:"Expiration"`
		}
		if err := json.Unmarshal(out, &c); err != nil {
			return backup.Credentials{}, fmt.Errorf("profile %s: credential_process output is not JSON", name)
		}
		if c.Version != 1 {
			return backup.Credentials{}, fmt.Errorf("profile %s: credential_process Version is %d, want 1", name, c.Version)
		}
		cc := containerCreds{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, Token: c.SessionToken, Expiration: c.Expiration}
		return cc.credentials("profile " + name + " (credential_process)")
	})
}

// splitCommand splits a command line on spaces, honouring single and double quotes and
// backslash escapes, the way the AWS CLI passes credential_process to the system.
func splitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord, quote, escaped := false, rune(0), false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped, inWord = false, true
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}
