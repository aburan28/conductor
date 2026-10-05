package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/adamburan/conductor/internal/githubapp"
	"github.com/adamburan/conductor/internal/secretbox"
)

// How the GitHub App's credentials are stored in github_app.credentials.
//
// The row keeps the app's public identity (id, slug, name, owner, URLs) as plain JSON, so an
// operator can still see which app a database is configured for, and replaces the three
// secrets — the private key, the webhook secret and the OAuth client secret — with one
// "sealed" field: their JSON, sealed under conductord's secret key (internal/secretbox),
// which is never stored in the database. A database backup therefore cannot sign as the app
// or forge its webhooks.
//
// Rows written before sealing hold the secrets in plain JSON. They are still read, and are
// sealed in place the first time a server with a key loads them.

// sealPurpose binds the sealed value to this use.
const sealPurpose = "conductor/github_app/credentials"

// secretFields are the Credentials JSON keys that are sealed.
var secretFields = []string{"pem", "webhook_secret", "client_secret"}

type appSecrets struct {
	PrivateKeyPEM string `json:"pem"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
}

// sealCredentials renders creds for storage with its secrets sealed.
func sealCredentials(src *secretbox.Source, creds githubapp.Credentials) ([]byte, error) {
	if src == nil {
		return nil, errors.New("no secret key is configured to seal the GitHub App's credentials")
	}
	key, err := src.Get(true)
	if err != nil {
		return nil, err
	}
	secrets, err := json.Marshal(appSecrets{creds.PrivateKeyPEM, creds.WebhookSecret, creds.ClientSecret})
	if err != nil {
		return nil, err
	}
	sealed, err := key.Seal(secrets, sealPurpose)
	if err != nil {
		return nil, err
	}
	public, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	var row map[string]any
	if err := json.Unmarshal(public, &row); err != nil {
		return nil, err
	}
	for _, f := range secretFields {
		delete(row, f)
	}
	row["sealed"] = sealed
	return json.Marshal(row)
}

// openCredentials reads a stored row. plaintext reports a row written before sealing, which
// the caller should seal.
func openCredentials(src *secretbox.Source, raw []byte) (creds githubapp.Credentials, plaintext bool, err error) {
	var row struct {
		Sealed string `json:"sealed"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return creds, false, fmt.Errorf("stored github app: %w", err)
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return creds, false, fmt.Errorf("stored github app: %w", err)
	}
	if row.Sealed == "" {
		return creds, creds.PrivateKeyPEM != "" || creds.WebhookSecret != "" || creds.ClientSecret != "", nil
	}
	cannot := func(err error) error {
		return fmt.Errorf("the GitHub App's stored credentials cannot be unsealed: %w. "+
			"Every conductord sharing this database must use the same secret key (--secret-key-file or %s; docs/OPERATIONS.md). "+
			"If that key is lost, connect the app again with `conductor github setup --replace`",
			err, secretbox.EnvKey)
	}
	if src == nil {
		return creds, false, cannot(errors.New("no secret key is configured"))
	}
	key, err := src.Get(false)
	if err != nil {
		return creds, false, cannot(fmt.Errorf("they were sealed with key %s, and %w", secretbox.SealedKeyID(row.Sealed), err))
	}
	plain, err := key.Open(row.Sealed, sealPurpose)
	if err != nil {
		return creds, false, cannot(err)
	}
	var secrets appSecrets
	if err := json.Unmarshal(plain, &secrets); err != nil {
		return creds, false, fmt.Errorf("stored github app secrets: %w", err)
	}
	creds.PrivateKeyPEM, creds.WebhookSecret, creds.ClientSecret = secrets.PrivateKeyPEM, secrets.WebhookSecret, secrets.ClientSecret
	return creds, false, nil
}
