package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	log "github.com/sirupsen/logrus"
)

func EncryptHandshake(plaintext string) (string, error) {
	key := []byte(os.Getenv("HANDSHAKE_SECRET"))
	log.Info(string(key))
	if len(key) != 32 {
		return "", fmt.Errorf("ключ должен быть 32 байта")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := aesgcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}
