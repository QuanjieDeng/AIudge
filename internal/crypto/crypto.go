// Package crypto 用 AES-256-GCM 加密落库的凭据（仓库 token、网关密钥）。
// 密文格式 base64(nonce || ciphertext)，附加认证不绑定 AAD，密钥轮换时整体重写即可。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

var ErrNoKey = errors.New("未配置加密密钥")

func Encrypt(key []byte, plaintext string) (string, error) {
	if len(key) == 0 {
		return "", ErrNoKey
	}
	if plaintext == "" {
		return "", nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// Decrypt 空串视为「未配置」，直接返回空，让调用方少写一个分支。
func Decrypt(key []byte, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if len(key) == 0 {
		return "", ErrNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文长度不足")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// Mask 给界面回显用。凭据永不明文出库（§5），只给末 4 位证明「存进去了」。
func Mask(plaintext string) string {
	if len(plaintext) <= 4 {
		return "****"
	}
	return "****" + plaintext[len(plaintext)-4:]
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
