package business

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewRegistryFromYAML_LoadRealFile 加载项目实际配置文件
//   路径: configs/mappings.yaml (从测试文件相对路径算)
func TestNewRegistryFromYAML_LoadRealFile(t *testing.T) {
	// 找项目根 (测试文件在 internal/business/)
	path := findRepoRoot(t)
	yamlPath := filepath.Join(path, "configs", "mappings.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Skipf("mappings.yaml not found at %s, skip: %v", yamlPath, err)
	}

	reg, err := NewRegistryFromYAML(yamlPath)
	if err != nil {
		t.Fatalf("load yaml: %v", err)
	}

	// 验证 entity 数量
	ents := reg.List()
	if len(ents) < 2 {
		t.Fatalf("expected at least 2 entities, got %v", ents)
	}

	// 验证 products entity
	prod, ok := reg.Get("products")
	if !ok {
		t.Fatal("products entity missing")
	}
	if _, ok := prod.Fields["barcode"]; !ok {
		t.Error("products.barcode field missing")
	}
	if _, ok := prod.Fields["clsno"]; !ok {
		t.Error("products.clsno field missing (restock 收编必备)")
	}
	if _, ok := prod.Fields["unit"]; !ok {
		t.Error("products.unit field missing (restock 收编必备)")
	}

	// 验证 hbpos source
	hbpos, ok := prod.Sources["hbpos"]
	if !ok {
		t.Fatal("products.hbpos source missing")
	}
	if hbpos.Cube != "t_bd_item_info" {
		t.Errorf("products.hbpos.cube = %q, want t_bd_item_info", hbpos.Cube)
	}
	if hbpos.FieldRefs["clsno"] != "t_bd_item_info.item_clsno" {
		t.Errorf("products.hbpos.clsno mapping wrong: %q", hbpos.FieldRefs["clsno"])
	}

	// 验证 erp source
	erp, ok := prod.Sources["erp"]
	if !ok {
		t.Fatal("products.erp source missing")
	}
	if erp.FieldRefs["clsno"] != "" {
		t.Errorf("products.erp.clsno should be empty (ERP 未实现), got %q", erp.FieldRefs["clsno"])
	}

	// 验证 suppliers entity
	sup, ok := reg.Get("suppliers")
	if !ok {
		t.Fatal("suppliers entity missing")
	}
	if sup.Sources["hbpos"].FieldRefs["supplier_name"] != "suppliers.sup_name" {
		t.Error("suppliers.hbpos.supplier_name mapping wrong")
	}
}

// TestNewRegistryFromYAML_EquivDefaultRegistry 验证 YAML 加载结果跟 NewDefaultRegistry 等价
//   防止两份数据不一致(后续 CI 检查基础)
func TestNewRegistryFromYAML_EquivDefaultRegistry(t *testing.T) {
	yamlPath := filepath.Join(findRepoRoot(t), "configs", "mappings.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Skipf("mappings.yaml not found, skip: %v", err)
	}

	yamlReg, err := NewRegistryFromYAML(yamlPath)
	if err != nil {
		t.Fatalf("yaml: %v", err)
	}
	defReg := NewDefaultRegistry()

	// 比较 entity 清单
	yamlEnts := append([]string{}, yamlReg.List()...)
	defEnts := append([]string{}, defReg.List()...)
	if strings.Join(yamlEnts, ",") != strings.Join(defEnts, ",") {
		t.Errorf("entity list mismatch:\n  yaml:    %v\n  default: %v", yamlEnts, defEnts)
	}

	// 逐 entity 比 fields 和 sources
	for _, name := range defEnts {
		defEnt, _ := defReg.Get(name)
		yamlEnt, ok := yamlReg.Get(name)
		if !ok {
			t.Errorf("entity %q: missing in yaml", name)
			continue
		}
		// 比 field 数
		if len(yamlEnt.Fields) != len(defEnt.Fields) {
			t.Errorf("entity %q: field count mismatch: yaml=%d default=%d",
				name, len(yamlEnt.Fields), len(defEnt.Fields))
		}
		// 逐 field 比 type
		for fk, fDef := range defEnt.Fields {
			yDef, ok := yamlEnt.Fields[fk]
			if !ok {
				t.Errorf("entity %q: field %q missing in yaml", name, fk)
				continue
			}
			if yDef.Type != fDef.Type {
				t.Errorf("entity %q: field %q type mismatch: yaml=%s default=%s",
					name, fk, yDef.Type, fDef.Type)
			}
		}
		// 逐 source 比 cube
		for sk, sDef := range defEnt.Sources {
			ySrc, ok := yamlEnt.Sources[sk]
			if !ok {
				t.Errorf("entity %q: source %q missing in yaml", name, sk)
				continue
			}
			if ySrc.Cube != sDef.Cube {
				t.Errorf("entity %q source %q: cube mismatch: yaml=%s default=%s",
					name, sk, ySrc.Cube, sDef.Cube)
			}
			// 逐 field ref 比
			for fk, ref := range sDef.FieldRefs {
				if ySrc.FieldRefs[fk] != ref {
					t.Errorf("entity %q source %q field %q: ref mismatch: yaml=%q default=%q",
						name, sk, fk, ySrc.FieldRefs[fk], ref)
				}
			}
		}
	}
}

// TestNewRegistryFromYAMLBytes_InvalidYAML 错误格式要返 error
func TestNewRegistryFromYAMLBytes_InvalidYAML(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"no entities", "foo: bar\n"},
		{"entity no fields", "entities:\n  x:\n    sources:\n      y:\n        cube: c\n"},
		{"source no cube", "entities:\n  x:\n    fields:\n      a: {type: dimension}\n    sources:\n      y:\n        fields:\n          a: ref.a\n"},
		{"bad field type", `entities:
  x:
    fields:
      a: {type: wrongtype}
    sources:
      y:
        cube: c
        fields:
          a: ref.a
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewRegistryFromYAMLBytes([]byte(c.data))
			if err == nil {
				t.Errorf("expected error for %q, got nil", c.name)
			}
		})
	}
}

// findRepoRoot 找项目根 (有 go.mod 的目录)
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found in any parent dir")
	return ""
}
