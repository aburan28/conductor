package quota

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
)

// AccountIdentity is a hint from local profile metadata, never an authenticated assertion.
// The opaque key separates two logins that occupy the same state directory at different
// times. Email is used only when decorating the requesting owner's view.
type AccountIdentity struct {
	Key               string
	Email             string
	ProviderAccountID string // used only to match Codex rollout headers on this machine
	ProviderOrgID     string
}

// Identity reads only the identifiers needed to distinguish a local Claude or Codex login.
// Neither tokens nor the rest of the profile are retained or logged. These file layouts are
// observable local formats and can change, so absence means unknown identity.
func Identity(harness, stateDir, home string) AccountIdentity {
	switch harness {
	case "claude":
		path := filepath.Join(stateDir, ".claude.json")
		if filepath.Clean(stateDir) == filepath.Join(home, ".claude") {
			path = filepath.Join(home, ".claude.json")
		}
		var profile struct {
			OAuthAccount struct {
				Email string `json:"emailAddress"`
				ID    string `json:"accountUuid"`
				Org   string `json:"organizationUuid"`
			} `json:"oauthAccount"`
		}
		if readIdentityJSON(path, &profile) && profile.OAuthAccount.ID != "" {
			return AccountIdentity{Key: identityDigest("claude", profile.OAuthAccount.ID, profile.OAuthAccount.Org),
				Email: profile.OAuthAccount.Email, ProviderAccountID: profile.OAuthAccount.ID,
				ProviderOrgID: profile.OAuthAccount.Org}
		}
	case "codex":
		var auth struct {
			Tokens struct {
				IDToken   string `json:"id_token"`
				AccountID string `json:"account_id"`
			} `json:"tokens"`
		}
		if !readIdentityJSON(filepath.Join(stateDir, "auth.json"), &auth) || auth.Tokens.IDToken == "" {
			return AccountIdentity{}
		}
		parts := strings.Split(auth.Tokens.IDToken, ".")
		if len(parts) != 3 {
			return AccountIdentity{}
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil || len(payload) > 64<<10 {
			return AccountIdentity{}
		}
		var claims struct {
			Subject string `json:"sub"`
			Email   string `json:"email"`
			Auth    struct {
				AccountID string `json:"chatgpt_account_id"`
			} `json:"https://api.openai.com/auth"`
		}
		if json.Unmarshal(payload, &claims) != nil || claims.Subject == "" {
			return AccountIdentity{}
		}
		accountID := claims.Auth.AccountID
		if accountID == "" {
			accountID = auth.Tokens.AccountID
		}
		if accountID == "" {
			return AccountIdentity{}
		}
		return AccountIdentity{Key: identityDigest("codex", claims.Subject, accountID),
			Email: claims.Email, ProviderAccountID: accountID}
	}
	return AccountIdentity{}
}

func readIdentityJSON(path string, out any) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 2<<20 {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, out) == nil
}

func identityDigest(harness string, ids ...string) string {
	sum := sha256.Sum256([]byte(harness + "\x00" + strings.Join(ids, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// AccountKey is the quota storage key. The human profile label remains its prefix, while
// a credential switch in that profile produces a different quota and alert key.
func AccountKey(harness, stateDir, home string) string {
	label := AccountLabel(harness, stateDir, home)
	if id := Identity(harness, stateDir, home).Key; id != "" {
		const suffix = "-id-"
		if len(label) > 64-len(suffix)-len(id) {
			label = label[:64-len(suffix)-len(id)]
		}
		return label + suffix + id
	}
	return label
}

// IdentitySlot is the state-directory label in an identity-bound quota key.
// A hint for one login is never attached to the old key after that slot switches accounts.
func IdentitySlot(account string) (string, bool) {
	i := strings.LastIndex(account, "-id-")
	if i < 1 || len(account)-i != len("-id-")+12 {
		return "", false
	}
	suffix := account[i+len("-id-"):]
	if _, err := hex.DecodeString(suffix); err != nil || strings.ToLower(suffix) != suffix {
		return "", false
	}
	return account[:i], true
}

// ValidIdentityEmail accepts a bare mailbox address for display, with a bounded payload.
func ValidIdentityEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email
}

// CurrentIdentity returns the email for a key only if the requesting server is reading the
// same machine and the current local profile still resolves to that key. The email is never
// stored with the quota snapshot or returned in a project-wide count or event.
func CurrentIdentity(harness, account, machine, home string, getenv func(string) string) (string, bool) {
	host, err := os.Hostname()
	if err != nil || host != machine || (harness != "claude" && harness != "codex") {
		return "", false
	}
	for _, dir := range AccountDirs(harness, home, getenv) {
		if AccountKey(harness, dir, home) == account {
			id := Identity(harness, dir, home)
			return id.Email, id.Key != ""
		}
	}
	return "", false
}
