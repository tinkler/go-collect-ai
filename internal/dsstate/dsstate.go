// Package dsstate 数据源状态持久化
//   写入 ./datasource.state 文件
//   SetDataSource 调用 Save, 启动时 Load override cfg 默认值
//   2026-08-29 加入, 解决 collect-ai 重启后 datasource 丢失问题
package dsstate

import (
	"errors"
	"os"
	"strings"
)

const DefaultPath = "./datasource.state"

// Load 读持久化的 ds; 不存在 / 非法值返回 ""
func Load(path string) string {
	if path == "" {
		path = DefaultPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	ds := strings.ToLower(strings.TrimSpace(string(b)))
	if ds != "erp" && ds != "hbpos" {
		return ""
	}
	return ds
}

// Save 写持久化的 ds
func Save(path, ds string) error {
	if path == "" {
		path = DefaultPath
	}
	ds = strings.ToLower(strings.TrimSpace(ds))
	if ds != "erp" && ds != "hbpos" {
		return errors.New("invalid ds: " + ds)
	}
	return os.WriteFile(path, []byte(ds), 0o644)
}
