// Package business - 从 YAML 加载业务字段映射
//
// 设计动机(2026-09-02):
//   原 NewDefaultRegistry() hardcode 业务字段映射在 Go 代码里
//   → 加新 datasource (kingdee/yonyou/...) 要改 Go 代码 + 重启
//   → 不适合后续多类型 datasource 部署
//
//   现在支持 NewRegistryFromYAML(path) 从 configs/mappings.yaml 加载
//   → 加 datasource 改 yaml + 重启(或后续接 hot reload)
//   → Go 代码零改动
//
// 向后兼容:
//   NewDefaultRegistry() 保留,加载 yaml 失败时 main.go 可 fallback
//   两份数据要保持同步(后续可加 CI 检查)
package business

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// yamlConfig 顶层 YAML 结构
type yamlConfig struct {
	Entities map[string]yamlEntity `yaml:"entities"`
}

// yamlEntity 单个 entity YAML
type yamlEntity struct {
	Name        string                  `yaml:"name"`
	Description string                  `yaml:"description"`
	Fields      map[string]yamlFieldDef `yaml:"fields"`
	Sources     map[string]yamlSource   `yaml:"sources"`
}

// yamlFieldDef 字段定义 YAML
type yamlFieldDef struct {
	Type        string            `yaml:"type"`
	Description string            `yaml:"description"`
	Required    bool              `yaml:"required"`
	// ValueMap 业务值 → 物理值翻译表 (W4.4, 2026-09-04)
	//   例: status: {pending: "0", approved: "1"}
	//   filter 传 {field:"status", op:"equals", values:["pending"]}
	//   自动翻成 {member:"approve_flag", op:"equals", values:["0"]}
	ValueMap map[string]string `yaml:"value_map,omitempty"`
}

// yamlSource 单数据源 YAML
type yamlSource struct {
	Cube   string            `yaml:"cube"`
	Fields map[string]string `yaml:"fields"`
}

// NewRegistryFromYAML 从 YAML 文件加载 Registry
//
//	启动期调用一次,运行时只读
//	加载失败返 error(让调用方决定 fallback 到 NewDefaultRegistry 还是 fatal)
//
//	YAML schema 见 configs/mappings.yaml 顶部注释
func NewRegistryFromYAML(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mappings yaml %s: %w", path, err)
	}
	return NewRegistryFromYAMLBytes(data)
}

// NewRegistryFromYAMLBytes 从字节数组加载 (单测用,不用真实文件)
func NewRegistryFromYAMLBytes(data []byte) (*Registry, error) {
	var cfg yamlConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal mappings yaml: %w", err)
	}
	if len(cfg.Entities) == 0 {
		return nil, fmt.Errorf("mappings yaml: no entities defined")
	}

	r := &Registry{entities: map[string]*EntityMapping{}}
	for entKey, ye := range cfg.Entities {
		ent, err := buildEntity(entKey, ye)
		if err != nil {
			return nil, fmt.Errorf("entity %q: %w", entKey, err)
		}
		r.entities[entKey] = ent
	}
	return r, nil
}

// buildEntity 把 yamlEntity 转成 EntityMapping
func buildEntity(defaultName string, ye yamlEntity) (*EntityMapping, error) {
	if len(ye.Fields) == 0 {
		return nil, fmt.Errorf("no fields defined")
	}

	fields := make(map[string]FieldDef, len(ye.Fields))
	for fk, yf := range ye.Fields {
		ft, err := parseFieldType(yf.Type)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", fk, err)
		}
		fields[fk] = FieldDef{
			Name:        fk, // 业务字段名跟 key 一致
			Type:        ft,
			Required:    yf.Required,
			Description: yf.Description,
			ValueMap:    yf.ValueMap, // W4.4: 业务值→物理值翻译表 (空/nil = 不翻译)
		}
	}

	sources := make(map[string]SourceMapping, len(ye.Sources))
	for sk, ys := range ye.Sources {
		if ys.Cube == "" {
			return nil, fmt.Errorf("source %q: cube is empty", sk)
		}
		if len(ys.Fields) == 0 {
			return nil, fmt.Errorf("source %q: no fields mapped", sk)
		}
		// AvailableFields 从 FieldRefs 里推断 (按 yaml 顺序保留)
		avail := make([]string, 0, len(ys.Fields))
		for fk := range ys.Fields {
			avail = append(avail, fk)
		}
		sources[sk] = SourceMapping{
			Cube:            ys.Cube,
			FieldRefs:       ys.Fields,
			AvailableFields: avail,
		}
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources defined")
	}

	name := ye.Name
	if name == "" {
		name = defaultName
	}
	return &EntityMapping{
		Name:        name,
		Description: ye.Description,
		Fields:      fields,
		Sources:     sources,
	}, nil
}

// parseFieldType 解析字段类型字符串
func parseFieldType(s string) (FieldType, error) {
	switch s {
	case "dimension", "":
		return FieldTypeDimension, nil
	case "measure":
		return FieldTypeMeasure, nil
	case "time":
		return FieldTypeTime, nil
	default:
		return "", fmt.Errorf("invalid field type %q (want dimension/measure/time)", s)
	}
}
