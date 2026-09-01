package supplierpayment

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// findRepoRoot 用 runtime.Caller 找 .env 标志
func findRepoRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// getEnvDirect 直读 os.Getenv (避免与 main 包同名冲突)
func getEnvDirect(k string) string {
	return os.Getenv(k)
}

// testReadEnvFile 读 .env 文件 (在 cwd 找)
func testReadEnvFile(path string) string {
	dsn, _ := readEnvFileRaw(path)
	return dsn
}

// testReadEnvFileFromRoot 在 repo 根找 .env
func testReadEnvFileFromRoot() string {
	root := findRepoRoot()
	if root == "" {
		return ""
	}
	dsn, _ := readEnvFileRaw(filepath.Join(root, ".env"))
	return dsn
}

// readEnvFileRaw 读 .env, 返回 PG DSN
func readEnvFileRaw(path string) (string, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(bs), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 跳过非 ASCII (中文 header)
		if !isASCII(line) {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		m[strings.TrimSpace(line[:eq])] = strings.TrimSpace(line[eq+1:])
	}
	host, user, db := m["PG_HOST"], m["PG_USER"], m["PG_DATABASE"]
	port := m["PG_PORT"]
	if port == "" {
		port = "5432"
	}
	if host == "" || user == "" || db == "" {
		return "", os.ErrNotExist
	}
	return "postgres://" + user + ":" + m["PG_PASSWORD"] + "@" + host + ":" + port + "/" + db + "?sslmode=disable", nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// 防止 testhelper_test.go 在无测试函数时 build 失败
var _ = testing.Short
