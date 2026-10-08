package pgarchive

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aburan28/conductor/internal/backup"
)

// A cluster's archive is sealed under one random data key. The key itself is kept in the
// bucket sealed with the passphrase (PBKDF2-SHA256, then AES-256-GCM), so any machine with
// the passphrase can restore, and cached on this machine in the clear (0600 in a 0700
// directory, beside the CLI's credentials) so archive-wal, which Postgres runs for every
// segment, does not pay for a key derivation each time.

const (
	kdfIterations = 600_000
	keyAD         = "conductor db key v1"
)

// keyFile is <base>/key.json in the bucket.
type keyFile struct {
	Version    int    `json:"version"`
	KeyID      string `json:"key_id"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	SealedKey  string `json:"sealed_key"`
}

// ErrNoPassphrase means sealing is on but no passphrase is configured.
var ErrNoPassphrase = errors.New("sealing is on but there is no seal passphrase (CONDUCTOR_CHECKPOINT_KEY, or the app's Storage settings)")

func kek(passphrase string, salt []byte, iterations int) (cipher.AEAD, error) {
	k, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealKey(key *DataKey, passphrase string) (keyFile, error) {
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return keyFile{}, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return keyFile{}, err
	}
	aead, err := kek(passphrase, salt, kdfIterations)
	if err != nil {
		return keyFile{}, err
	}
	ct := aead.Seal(nil, nonce, key.raw[:], []byte(keyAD+key.ID()))
	return keyFile{
		Version: 1, KeyID: key.ID(), KDF: "pbkdf2-sha256", Iterations: kdfIterations,
		Salt: base64.StdEncoding.EncodeToString(salt), SealedKey: base64.StdEncoding.EncodeToString(append(nonce, ct...)),
	}, nil
}

func openKey(f keyFile, passphrase string) (*DataKey, error) {
	if f.Version != 1 || f.KDF != "pbkdf2-sha256" || f.Iterations < 100_000 {
		return nil, fmt.Errorf("unsupported archive key file (version %d, kdf %s)", f.Version, f.KDF)
	}
	salt, err := base64.StdEncoding.DecodeString(f.Salt)
	if err != nil {
		return nil, err
	}
	sealed, err := base64.StdEncoding.DecodeString(f.SealedKey)
	if err != nil || len(sealed) < 12 {
		return nil, errors.New("the archive key file is corrupt")
	}
	aead, err := kek(passphrase, salt, f.Iterations)
	if err != nil {
		return nil, err
	}
	raw, err := aead.Open(nil, sealed[:12], sealed[12:], []byte(keyAD+f.KeyID))
	if err != nil {
		return nil, errors.New("the seal passphrase does not open this archive's key")
	}
	key, err := NewDataKey(raw)
	if err != nil {
		return nil, err
	}
	if key.ID() != f.KeyID {
		return nil, errors.New("the archive key does not match its recorded ID")
	}
	return key, nil
}

// keyCachePath is <state>/db-keys/<system id>.key.
func (a *Archiver) keyCachePath() string {
	return filepath.Join(a.cfg.StateDir, "db-keys", a.systemID+".key")
}

// dataKey returns the cluster's data key: from the local cache, else unsealed from the
// bucket with the passphrase, else (when create is true) newly generated and stored.
func (a *Archiver) dataKey(ctx context.Context, create bool) (*DataKey, error) {
	if a.key != nil {
		return a.key, nil
	}
	if b, err := os.ReadFile(a.keyCachePath()); err == nil {
		raw, err := base64.StdEncoding.DecodeString(string(b))
		if err == nil {
			if key, err := NewDataKey(raw); err == nil {
				a.key = key
				return key, nil
			}
		}
	}
	pass := ""
	if a.cfg.Passphrase != nil {
		p, err := a.cfg.Passphrase(ctx)
		if err != nil {
			return nil, err
		}
		pass = p
	}
	if pass == "" {
		return nil, ErrNoPassphrase
	}
	body, err := a.s3.Get(ctx, a.keyKey())
	switch {
	case err == nil:
		var f keyFile
		if err := json.Unmarshal(body, &f); err != nil {
			return nil, fmt.Errorf("the archive key file is corrupt: %w", err)
		}
		key, err := openKey(f, pass)
		if err != nil {
			return nil, err
		}
		return a.cacheKey(key)
	case !errors.Is(err, backup.ErrNotFound):
		return nil, err
	case !create:
		return nil, errors.New("this cluster's archive has no key yet (nothing sealed has been archived)")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	key, _ := NewDataKey(raw)
	f, err := sealKey(key, pass)
	if err != nil {
		return nil, err
	}
	body, _ = json.MarshalIndent(f, "", "  ")
	if err := a.s3.PutIfAbsent(ctx, a.keyKey(), body, "application/json"); errors.Is(err, backup.ErrExists) {
		// Another archiver won the race; use its key.
		a.key = nil
		return a.dataKey(ctx, false)
	} else if err != nil {
		return nil, err
	}
	return a.cacheKey(key)
}

func (a *Archiver) cacheKey(key *DataKey) (*DataKey, error) {
	path := a.keyCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, []byte(base64.StdEncoding.EncodeToString(key.raw[:])), 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}
	a.key = key
	return key, nil
}
