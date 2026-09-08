package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// randHex 生成 n 字节随机 hex (2n 个字符)
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败是非常罕见的系统级错误, 直接 panic
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
