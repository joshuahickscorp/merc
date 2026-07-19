package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"strings"
)

func tokenKey() []byte {
	k := os.Getenv("CX_TOKEN_KEY")
	if k == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(k))
	return sum[:]
}

func sealToken(plaintext string) string {
	key := tokenKey()
	if key == nil {
		return "plain:" + plaintext
	}
	return sealTokenWithReader(plaintext, key, rand.Reader)
}

func sealTokenWithReader(plaintext string, key []byte, random io.Reader) string {
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return ""
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:" + base64.RawStdEncoding.EncodeToString(ct)
}

func openToken(stored string) string {
	if strings.HasPrefix(stored, "plain:") {
		return strings.TrimPrefix(stored, "plain:")
	}
	if !strings.HasPrefix(stored, "enc:") {
		return stored
	}
	key := tokenKey()
	if key == nil {
		return ""
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, "enc:"))
	if err != nil {
		return ""
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(raw) < gcm.NonceSize() {
		return ""
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return ""
	}
	return string(pt)
}

func newSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return ""
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
