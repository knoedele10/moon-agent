package model

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

var errNoSecret = errors.New("APP_SECRET is not set; cannot derive encryption key")

func secretKey() ([]byte, error) {
	secret := os.Getenv("APP_SECRET")
	if secret == "" {
		return nil, errNoSecret
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

func newGCM() (cipher.AEAD, error) {
	key, err := secretKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func EncryptKey(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	gcm, err := newGCM()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func DecryptKey(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	gcm, err := newGCM()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", errors.New("decrypt key failed: invalid base64 ciphertext")
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("decrypt key failed: ciphertext too short")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("decrypt key failed: authentication error (wrong APP_SECRET or corrupted data)")
	}
	return string(plain), nil
}
