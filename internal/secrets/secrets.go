// Package secrets encrypts small values (API tokens, passwords) before
// they are written to disk. The key lives in its own file next to the
// database, readable by the owner only.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "v1:"

// Box seals and opens values with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// OpenBox loads the key from path, creating a new random key the first
// time.
func OpenBox(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		// O_EXCL: never overwrite a key someone else just created.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s: key must be 32 bytes, found %d", path, len(key))
	}
	return NewBox(key)
}

// NewBox uses a 32-byte key directly.
func NewBox(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plain text.
func (b *Box) Seal(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(ct), nil
}

// Open decrypts a value made by Seal.
func (b *Box) Open(sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return "", errors.New("not a sealed value")
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(data) < n {
		return "", errors.New("sealed value too short")
	}
	plain, err := b.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return "", errors.New("could not decrypt (wrong key?)")
	}
	return string(plain), nil
}

// IsSealed reports whether s looks like a sealed value.
func IsSealed(s string) bool { return strings.HasPrefix(s, prefix) }
