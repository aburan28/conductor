package pgarchive

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Sealed objects are encrypted on this machine before they reach the bucket. The format is
// a STREAM-style chunked AEAD, so a multi-gigabyte base backup is sealed and opened without
// ever being held in memory:
//
//	header  = magic (8) | key ID (8) | salt (16)
//	chunk   = length (4, big-endian; top bit marks the final chunk) | AES-256-GCM ciphertext
//
// Each object gets its own key, HMAC-SHA256(data key, label | salt), so nonces (a 64-bit
// chunk counter) never repeat under one key. The header and the chunk's length word are the
// associated data, so a chunk cannot be moved, a non-final chunk cannot pose as the last
// one, and a truncated object fails to open instead of opening short.

const (
	sealMagic   = "CDARCH1\n"
	sealChunk   = 1 << 20
	finalBit    = 1 << 31
	headerLen   = 8 + 8 + 16
	objectLabel = "conductor db archive object v1"
)

// ErrWrongKey is returned when an object's key ID is not the key in hand.
var ErrWrongKey = errors.New("this object was sealed with a different key")

// DataKey is the 32-byte key a cluster's archive is sealed with.
type DataKey struct {
	raw [32]byte
	id  [8]byte
}

// NewDataKey wraps raw key material.
func NewDataKey(raw []byte) (*DataKey, error) {
	if len(raw) != 32 {
		return nil, fmt.Errorf("a data key is 32 bytes, not %d", len(raw))
	}
	k := &DataKey{}
	copy(k.raw[:], raw)
	sum := sha256.Sum256(append([]byte("conductor db key id"), raw...))
	copy(k.id[:], sum[:8])
	return k, nil
}

// ID is a short public identifier of the key, safe to store beside sealed objects.
func (k *DataKey) ID() string { return fmt.Sprintf("%x", k.id) }

func (k *DataKey) objectAEAD(salt []byte) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, k.raw[:])
	mac.Write([]byte(objectLabel))
	mac.Write(salt)
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(n uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], n)
	return nonce
}

// sealReader seals what it reads from src.
type sealReader struct {
	src     io.Reader
	aead    cipher.AEAD
	header  []byte
	counter uint64
	out     bytes.Buffer
	peek    []byte // one byte read ahead, to know whether a chunk is the last
	done    bool
	err     error
}

// Seal returns a reader that yields src sealed under key.
func Seal(src io.Reader, key *DataKey) (io.Reader, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := key.objectAEAD(salt)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, headerLen)
	header = append(header, sealMagic...)
	header = append(header, key.id[:]...)
	header = append(header, salt...)
	r := &sealReader{src: src, aead: aead, header: header}
	r.out.Write(header)
	return r, nil
}

func (r *sealReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		r.fill()
	}
	return r.out.Read(p)
}

// fill seals the next chunk into r.out.
func (r *sealReader) fill() {
	buf := make([]byte, sealChunk)
	n := copy(buf, r.peek)
	r.peek = nil
	m, err := io.ReadFull(r.src, buf[n:])
	n += m
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		r.err = err
		return
	}
	final := err != nil // the source ended inside (or exactly at) this chunk
	if !final {
		one := make([]byte, 1)
		k, err := io.ReadFull(r.src, one)
		switch {
		case k == 1:
			r.peek = one
		case errors.Is(err, io.EOF):
			final = true
		default:
			r.err = err
			return
		}
	}
	length := uint32(n + r.aead.Overhead())
	if final {
		length |= finalBit
	}
	word := binary.BigEndian.AppendUint32(nil, length)
	ad := append(append([]byte{}, r.header...), word...)
	r.out.Write(word)
	r.out.Write(r.aead.Seal(nil, chunkNonce(r.counter), buf[:n], ad))
	r.counter++
	if final {
		r.done = true
	}
}

// openReader opens a sealed stream.
type openReader struct {
	src     io.Reader
	aead    cipher.AEAD
	header  []byte
	counter uint64
	out     bytes.Buffer
	done    bool
	err     error
}

// Open returns a reader that yields the plaintext of the sealed stream src. Reading fails if
// the stream was sealed under another key, was altered, or ends before its final chunk.
func Open(src io.Reader, key *DataKey) (io.Reader, error) {
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(src, header); err != nil {
		return nil, fmt.Errorf("not a sealed archive object: %w", err)
	}
	if string(header[:8]) != sealMagic {
		return nil, errors.New("not a sealed archive object")
	}
	if !bytes.Equal(header[8:16], key.id[:]) {
		return nil, fmt.Errorf("%w (object key %x, have %s)", ErrWrongKey, header[8:16], key.ID())
	}
	aead, err := key.objectAEAD(header[16:])
	if err != nil {
		return nil, err
	}
	return &openReader{src: src, aead: aead, header: header}, nil
}

// IsSealed reports whether b begins like a sealed object.
func IsSealed(b []byte) bool { return bytes.HasPrefix(b, []byte(sealMagic)) }

func (r *openReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		r.next()
	}
	return r.out.Read(p)
}

func (r *openReader) next() {
	word := make([]byte, 4)
	if _, err := io.ReadFull(r.src, word); err != nil {
		r.err = errors.New("the sealed object is truncated")
		return
	}
	length := binary.BigEndian.Uint32(word)
	final := length&finalBit != 0
	length &^= finalBit
	if length < uint32(r.aead.Overhead()) || length > sealChunk+uint32(r.aead.Overhead()) {
		r.err = errors.New("the sealed object is corrupt (bad chunk length)")
		return
	}
	ct := make([]byte, length)
	if _, err := io.ReadFull(r.src, ct); err != nil {
		r.err = errors.New("the sealed object is truncated")
		return
	}
	ad := append(append([]byte{}, r.header...), word...)
	plain, err := r.aead.Open(nil, chunkNonce(r.counter), ct, ad)
	if err != nil {
		r.err = errors.New("the sealed object failed authentication (altered, or another key)")
		return
	}
	r.counter++
	r.out.Write(plain)
	if final {
		r.done = true
		// Nothing may follow the final chunk.
		if n, _ := r.src.Read(make([]byte, 1)); n > 0 {
			r.err = errors.New("the sealed object has data after its final chunk")
			r.out.Reset()
		}
	}
}
