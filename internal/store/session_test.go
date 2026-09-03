package store

import (
	"strings"
	"testing"
)

// ============================================================
// W4.1: HashImageBytes 重复图去重
// ============================================================

func TestHashImageBytes_Deterministic(t *testing.T) {
	b := []byte("hello world")
	h1 := HashImageBytes(b)
	h2 := HashImageBytes(b)
	if h1 != h2 {
		t.Errorf("相同 bytes 应有相同 hash, got %s vs %s", h1, h2)
	}
	// SHA-256 hex 64 字符
	if len(h1) != 64 {
		t.Errorf("hash 长度 = %d, want 64", len(h1))
	}
}

func TestHashImageBytes_DifferentBytes(t *testing.T) {
	a := []byte("image A")
	b := []byte("image B")
	ha := HashImageBytes(a)
	hb := HashImageBytes(b)
	if ha == hb {
		t.Error("不同 bytes 应有不同 hash")
	}
}

func TestHashImageBytes_Empty(t *testing.T) {
	h := HashImageBytes([]byte{})
	// SHA-256("") 是固定值
	if h != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty hash 不匹配预期 SHA-256 值, got %s", h)
	}
}

func TestHashImageBytes_KnownVector(t *testing.T) {
	// "abc" 的 SHA-256 已知
	h := HashImageBytes([]byte("abc"))
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if h != want {
		t.Errorf("got %s, want %s", h, want)
	}
}

func TestHashImageBytes_HexOnly(t *testing.T) {
	h := HashImageBytes([]byte("test"))
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("hash 含非 hex 字符: %q", string(c))
			break
		}
	}
}

// ============================================================
// W4.1: ImageCandidate 结构
// ============================================================

func TestImageCandidate_Fields(t *testing.T) {
	c := ImageCandidate{
		Hash:     "abc123",
		FileName: "test.jpg",
		ImgBytes: []byte{0x01, 0x02},
	}
	if c.Hash != "abc123" || c.FileName != "test.jpg" || len(c.ImgBytes) != 2 {
		t.Errorf("ImageCandidate 字段丢失: %+v", c)
	}
}

// ============================================================
// W4.1: nullStr helper
// ============================================================

func TestNullStr(t *testing.T) {
	if nullStr("") != nil {
		t.Error("空串应返 nil")
	}
	if nullStr("x") != "x" {
		t.Error("非空应返原值")
	}
	if nullStr(strings.Repeat("a", 100)) != strings.Repeat("a", 100) {
		t.Error("长串应原样返")
	}
}
