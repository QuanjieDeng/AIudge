package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// 密码用 PBKDF2-HMAC-SHA256 存哈希。选它是因为 crypto/pbkdf2 在 Go 1.24+ 标准库里，
// 不用为了一张只有几个账号的表引进 argon2/bcrypt 依赖。
// 迭代次数取 OWASP 对 PBKDF2-SHA256 的推荐值。
const (
	pbkdf2Iter  = 600_000
	pbkdf2Salt  = 16
	pbkdf2KeyLn = 32
)

// HashPassword 产出 "pbkdf2-sha256$迭代次数$salt$hash" 格式的串。
// 迭代次数写进串里，将来调高不用重算老密码。
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2Salt)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, pbkdf2KeyLn)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword 用固定时间比较，避免按字节比较泄漏信息。
// 哈希格式不对时返回 false 而不是报错——调用方只关心「能不能进」。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// 内置管理员。首次启动自动创建，必须改密码之后才能用其他功能——
// 一个写死在代码里的口令配上能读源码、能发群消息的权限，不改就是给整个内网开门。
const (
	DefaultAdminUsername = "admin"
	DefaultAdminPassword = "123456"
)
