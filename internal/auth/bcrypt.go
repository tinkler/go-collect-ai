package auth

import (
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 适中 (default 10), refresh token 验签在热路径上, 太大影响 QPS
const bcryptCost = 10

// bcryptHash 哈希 (salt + cost 自带)
//   bcrypt 限制 72 bytes, refresh token 是 "rt.<jwt>" 远长于此
//   所以先 SHA-256 → 32 字节 (hex 64 字符) → 再 bcrypt
func bcryptHash(plain string) (string, error) {
	digest := sha256Hex(plain)
	b, err := bcrypt.GenerateFromPassword([]byte(digest), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// bcryptCompare 比对 (constant-time)
func bcryptCompare(hash, plain string) bool {
	digest := sha256Hex(plain)
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(digest)) == nil
}

// sha256Hex SHA-256 → hex 64 字符 (固定 64 bytes, bcrypt 安全)
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
