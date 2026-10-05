// Package secretbox seals small secrets that conductord stores in Postgres (the GitHub App's
// private key and webhook secret) with AES-256-GCM under a key that is kept outside the
// database.
//
// The point is that a database dump — a backup, a restored copy on a laptop, a replica
// handed to someone for debugging — no longer carries credentials that let its holder act as
// the app. The key lives in a file beside conductord's other state (0600 in a 0700
// directory), or in the CONDUCTOR_SECRET_KEY environment variable from a secret store, and
// every replica must be given the same one.
//
// A sealed value is "v1.<key id>.<base64url(nonce || ciphertext)>". The key id is a short,
// non-reversible fingerprint of the key, so a value sealed under another key is reported as
// exactly that rather than as corrupt data.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// KeySize is the key length: AES-256.
const KeySize = 32

const version = "v1"

// ErrWrongKey means a value was sealed under a different key than the one configured.
var ErrWrongKey = errors.New("sealed with a different secret key")

// Key is a loaded secret key.
type Key struct {
	raw    []byte
	id     string
	source string
}

// NewKey wraps raw key bytes; source describes where they came from, for error messages.
func NewKey(raw []byte, source string) (*Key, error) {
	if len(raw) != KeySize {
		return nil, fmt.Errorf("secret key from %s is %d bytes, want %d", source, len(raw), KeySize)
	}
	return &Key{raw: append([]byte(nil), raw...), id: keyID(raw), source: source}, nil
}

// ID is the key's fingerprint as it appears in sealed values.
func (k *Key) ID() string { return k.id }

// Source says where the key came from (a file path or an environment variable).
func (k *Key) Source() string { return k.source }

func keyID(raw []byte) string {
	sum := sha256.Sum256(append([]byte("conductor/secretbox/key-id\x00"), raw...))
	return hex.EncodeToString(sum[:4])
}

// Seal encrypts plaintext. purpose is bound into the ciphertext as additional data, so a
// value sealed for one use cannot be passed off as another.
func (k *Key) Seal(plaintext []byte, purpose string) (string, error) {
	aead, err := k.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := aead.Seal(nonce, nonce, plaintext, []byte(purpose))
	return version + "." + k.id + "." + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts a value Seal produced with the same key and purpose.
func (k *Key) Open(sealed, purpose string) ([]byte, error) {
	v, id, body, ok := split(sealed)
	if !ok || v != version {
		return nil, errors.New("not a sealed value this version understands")
	}
	if id != k.id {
		return nil, fmt.Errorf("%w: the value was sealed with key %s, and this server's key (%s) is %s",
			ErrWrongKey, id, k.source, k.id)
	}
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("sealed value is malformed: %w", err)
	}
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("sealed value is truncated")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(purpose))
	if err != nil {
		return nil, errors.New("sealed value failed authentication: it was altered, or sealed for another purpose")
	}
	return plain, nil
}

func (k *Key) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SealedKeyID returns the key id a sealed value names, or "" if it is not one.
func SealedKeyID(sealed string) string {
	_, id, _, ok := split(sealed)
	if !ok {
		return ""
	}
	return id
}

func split(sealed string) (v, id, body string, ok bool) {
	parts := strings.SplitN(sealed, ".", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// ---------------------------------------------------------------------------
// Where the key comes from
// ---------------------------------------------------------------------------

// EnvKey and EnvKeyFile are the environment variables Source reads.
const (
	EnvKey     = "CONDUCTOR_SECRET_KEY"
	EnvKeyFile = "CONDUCTOR_SECRET_KEY_FILE"
)

// DefaultKeyPath is where conductord keeps its key when nothing else is configured: beside
// its other state (CONDUCTOR_STATE_DIR, else ~/.conductor), never in a repository checkout.
func DefaultKeyPath() (string, error) {
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "secret.key"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "secret.key"), nil
}

// Source finds the key lazily: a server that never stores a secret never creates a key file.
// Env, when set, is the base64 key itself and wins; otherwise the key is read from Path,
// which is created (0600, 32 random bytes) the first time a secret is sealed.
type Source struct {
	Env  string
	Path string

	mu  sync.Mutex
	key *Key
}

// Get returns the key. With create, a missing key file is generated; without it, a missing
// file is an error that names the path (opening a sealed value with a fresh key could
// never succeed).
func (s *Source) Get(create bool) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		return s.key, nil
	}
	if s.Env != "" {
		raw, err := decodeKey(s.Env)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvKey, err)
		}
		k, err := NewKey(raw, EnvKey)
		if err != nil {
			return nil, err
		}
		s.key = k
		return k, nil
	}
	if s.Path == "" {
		return nil, errors.New("no secret key configured (set --secret-key-file or " + EnvKey + ")")
	}
	data, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		if !create {
			return nil, fmt.Errorf("secret key file %s does not exist", s.Path)
		}
		if err := createKeyFile(s.Path); err != nil {
			return nil, fmt.Errorf("create secret key file: %w", err)
		}
		data, err = os.ReadFile(s.Path)
	}
	if err != nil {
		return nil, err
	}
	raw, err := decodeKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	k, err := NewKey(raw, s.Path)
	if err != nil {
		return nil, err
	}
	s.key = k
	return k, nil
}

// GenerateKey returns a new key in the encoding the key file and CONDUCTOR_SECRET_KEY use.
func GenerateKey() (string, error) {
	raw := make([]byte, KeySize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func decodeKey(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("secret key is not base64: %w", err)
	}
	if len(raw) != KeySize {
		return nil, fmt.Errorf("secret key is %d bytes, want %d", len(raw), KeySize)
	}
	return raw, nil
}

// createKeyFile writes a new key without ever replacing an existing one: the file is
// written under a temporary name and hard-linked into place, which fails if another
// process created the key first — and then that key is the one used.
func createKeyFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := GenerateKey()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".secret-key-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(encoded + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}
