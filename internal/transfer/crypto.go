package transfer

import (
	"crypto/aes"
	"crypto/cipher"
)

// newGCM builds the AEAD used for sync frames from the key the pairing code derives.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
