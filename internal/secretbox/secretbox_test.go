package secretbox

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSealRoundTrip(t *testing.T) {
	src := &Source{Path: filepath.Join(t.TempDir(), "state", "secret.key")}
	k, err := src.Get(true)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := k.Seal([]byte("-----BEGIN RSA PRIVATE KEY-----"), "github_app")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "BEGIN") || !strings.HasPrefix(sealed, "v1."+k.ID()+".") || SealedKeyID(sealed) != k.ID() {
		t.Fatalf("sealed = %q", sealed)
	}
	again, _ := k.Seal([]byte("-----BEGIN RSA PRIVATE KEY-----"), "github_app")
	if again == sealed {
		t.Error("two seals of one value are identical; the nonce is not fresh")
	}
	plain, err := k.Open(sealed, "github_app")
	if err != nil || string(plain) != "-----BEGIN RSA PRIVATE KEY-----" {
		t.Fatalf("Open = %q, %v", plain, err)
	}
	if _, err := k.Open(sealed, "something_else"); err == nil {
		t.Error("a value opened under another purpose")
	}
	// Flip one bit of the decoded tag. Overwriting the last base64 characters instead is
	// sometimes no change at all: the final character carries padding bits the decoder
	// ignores, so about one run in 1024 the "tampered" value was the original.
	body := sealed[strings.LastIndex(sealed, ".")+1:]
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	tampered := strings.TrimSuffix(sealed, body) + base64.RawURLEncoding.EncodeToString(raw)
	if _, err := k.Open(tampered, "github_app"); err == nil {
		t.Error("a tampered value opened")
	}

	// The file is 0600 in a 0700 directory, and a second load reads the same key.
	if st, err := os.Stat(src.Path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Dir(src.Path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("key dir: %v %v", st, err)
	}
	k2, err := (&Source{Path: src.Path}).Get(false)
	if err != nil || k2.ID() != k.ID() {
		t.Errorf("reloaded key %v, %v", k2, err)
	}
}

func TestWrongKeyIsNamed(t *testing.T) {
	dir := t.TempDir()
	a, _ := (&Source{Path: filepath.Join(dir, "a.key")}).Get(true)
	b, _ := (&Source{Path: filepath.Join(dir, "b.key")}).Get(true)
	sealed, err := a.Seal([]byte("x"), "p")
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Open(sealed, "p")
	if !errors.Is(err, ErrWrongKey) || !strings.Contains(err.Error(), a.ID()) || !strings.Contains(err.Error(), b.ID()) ||
		!strings.Contains(err.Error(), filepath.Join(dir, "b.key")) {
		t.Errorf("wrong key error = %v", err)
	}
}

func TestSourceEnvAndMissingFile(t *testing.T) {
	encoded, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "secret.key")
	k, err := (&Source{Env: encoded, Path: path}).Get(true)
	if err != nil || k.Source() != EnvKey {
		t.Fatalf("env key = %v, %v", k, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a key file was created although the key came from the environment")
	}
	if _, err := (&Source{Env: "too-short"}).Get(true); err == nil {
		t.Error("a malformed environment key was accepted")
	}
	// Opening needs the key that sealed; a missing file is reported, not invented.
	if _, err := (&Source{Path: path}).Get(false); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("missing key file = %v", err)
	}
	if _, err := (&Source{}).Get(true); err == nil {
		t.Error("no key source configured, and no error")
	}
}

// Replicas starting together on a shared state directory end up with one key.
func TestConcurrentCreateAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := (&Source{Path: path}).Get(true)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = k.ID()
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent creation produced different keys: %v", ids)
		}
	}
}
