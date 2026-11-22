package transfer

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Bundles travel between machines, so they can be encrypted in transit with age — the same
// optional tool cct uses. Nothing here implements crypto itself: age is spawned only to
// encrypt or decrypt a local file, so the "nothing is uploaded" guarantee is unchanged.

const ageMagic = "age-encryption.org/v1"

// OpenOptions controls decryption when opening a bundle.
type OpenOptions struct {
	// Identity is an age private-key file. Passphrase asks age to prompt on the terminal.
	Identity   string
	Passphrase bool
}

// IsAgeFile reports whether p looks like an age ciphertext.
func IsAgeFile(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, len(ageMagic))
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return false, err
	}
	return string(buf[:n]) == ageMagic, nil
}

// OpenFile opens a bundle, decrypting it first when it is age-encrypted. The returned
// cleanup func removes the temporary plaintext and must be called by the caller.
func OpenFile(p string, opts OpenOptions) (*Bundle, func(), error) {
	enc, err := IsAgeFile(p)
	if err != nil {
		return nil, func() {}, err
	}
	if !enc {
		b, err := Open(p)
		return b, func() {}, err
	}
	if opts.Identity == "" && !opts.Passphrase {
		return nil, func() {}, ErrEncrypted
	}
	if !ageAvailable() {
		return nil, func() {}, fmt.Errorf("bundle is age-encrypted and the optional 'age' tool is not installed")
	}
	dir, err := os.MkdirTemp("", "conductor-transfer-*")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	plain := filepath.Join(dir, "bundle")
	args := []string{"--decrypt", "--output", plain}
	if opts.Identity != "" {
		args = append(args, "--identity", opts.Identity)
	}
	args = append(args, p)
	cmd := exec.Command("age", args...)
	cmd.Stdin = os.Stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("age: %s", strings.TrimSpace(stderr.String()))
	}
	b, err := Open(plain)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return b, cleanup, nil
}

// EncryptFile encrypts plain to plain+".age" and removes the plaintext, matching cct's
// behavior. recipients are age public keys; recipientsFile is an age recipients file.
func EncryptFile(plain string, recipients []string, recipientsFile string, passphrase bool) (string, error) {
	if !ageAvailable() {
		return "", fmt.Errorf("the optional 'age' tool is not installed; install it or write an unencrypted bundle")
	}
	if passphrase && (len(recipients) > 0 || recipientsFile != "") {
		return "", fmt.Errorf("--passphrase cannot be combined with --encrypt-to or --recipients-file")
	}
	if !passphrase && len(recipients) == 0 && recipientsFile == "" {
		return "", fmt.Errorf("no recipient: pass --encrypt-to or --passphrase")
	}
	out := plain + ".age"
	args := []string{"--output", out}
	if passphrase {
		args = append(args, "--passphrase")
	}
	for _, r := range recipients {
		args = append(args, "--recipient", r)
	}
	if recipientsFile != "" {
		args = append(args, "--recipients-file", recipientsFile)
	}
	args = append(args, plain)
	cmd := exec.Command("age", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(out)
		return "", fmt.Errorf("age: %s", strings.TrimSpace(stderr.String()))
	}
	if err := os.Remove(plain); err != nil {
		return out, fmt.Errorf("encrypted to %s but could not remove the plaintext: %w", out, err)
	}
	return out, nil
}

func ageAvailable() bool {
	_, err := exec.LookPath("age")
	return err == nil
}
