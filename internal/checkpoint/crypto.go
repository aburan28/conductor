package checkpoint

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// A checkpoint leaving the machine carries the conversation, so it leaves sealed:
// AES-256-GCM over the whole archive under a key derived from a passphrase with
// PBKDF2-SHA256. The standard library alone provides all of it, which keeps the binary
// dependency-free, the same reason the S3 client signs requests by hand. The format is
// deliberately plain so another tool can open a bundle without this package:
//
//	"CKPT1\n" | salt[16] | nonce[12] | AES-256-GCM(key, nonce, gzip-tar, aad="CKPT1")
//
// The passphrase is never stored. Lose it and the bundle is gone; that is the point.

var sealMagic = []byte("CKPT1\n")

const (
	sealSaltLen  = 16
	sealNonceLen = 12
	sealIters    = 600_000
)

// IsSealed reports whether data is an encrypted bundle.
func IsSealed(data []byte) bool { return bytes.HasPrefix(data, sealMagic) }

// Seal encrypts an archive under a passphrase.
func Seal(plain []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("a passphrase is required to seal a checkpoint")
	}
	salt := make([]byte, sealSaltLen)
	nonce := make([]byte, sealNonceLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aead, err := sealAEAD(passphrase, salt)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(sealMagic)+sealSaltLen+sealNonceLen+len(plain)+aead.Overhead())
	out = append(out, sealMagic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, sealMagic), nil
}

// Unseal decrypts what Seal produced. A wrong passphrase and a tampered bundle are the same
// error: GCM cannot tell them apart, and neither should be opened.
func Unseal(data []byte, passphrase string) ([]byte, error) {
	if !IsSealed(data) {
		return nil, errors.New("not a sealed checkpoint")
	}
	rest := data[len(sealMagic):]
	if len(rest) < sealSaltLen+sealNonceLen {
		return nil, errors.New("sealed checkpoint is truncated")
	}
	salt, nonce, ct := rest[:sealSaltLen], rest[sealSaltLen:sealSaltLen+sealNonceLen], rest[sealSaltLen+sealNonceLen:]
	aead, err := sealAEAD(passphrase, salt)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, ct, sealMagic)
	if err != nil {
		return nil, errors.New("could not decrypt: wrong passphrase, or the bundle was altered")
	}
	return plain, nil
}

func sealAEAD(passphrase string, salt []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, sealIters, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
