package driver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// schemaDriver 实现 Driver + SchemaProvider。
type schemaDriver struct{}

func (schemaDriver) Open(context.Context, OpenRequest) (Conn, error) { return nil, nil }
func (schemaDriver) ConfigSchema() []Field {
	return []Field{{Name: "endpoint", Label: "端点", Type: FieldString, Required: true}}
}
func (schemaDriver) ParamSchema() []Field {
	return []Field{{Name: "slaveId", Label: "从机", Type: FieldInt, Default: 1}}
}

// plainDriver 仅实现 Driver,不实现 SchemaProvider。
type plainDriver struct{}

func (plainDriver) Open(context.Context, OpenRequest) (Conn, error) { return nil, nil }

func TestListReturnsSchema(t *testing.T) {
	Register("schemadriver", schemaDriver{})

	found := false
	for _, info := range List() {
		if info.Name != "schemadriver" {
			continue
		}
		found = true
		if len(info.Config) != 1 || info.Config[0].Name != "endpoint" || !info.Config[0].Required {
			t.Fatalf("config schema: %+v", info.Config)
		}
		if len(info.Params) != 1 || info.Params[0].Type != FieldInt || info.Params[0].Default != 1 {
			t.Fatalf("param schema: %+v", info.Params)
		}
	}
	if !found {
		t.Fatal("schemadriver not listed")
	}
}

// TestFieldShowWhenJSON 验证 ShowWhen 条件能正确序列化给前端(驱动条件显示)。
func TestFieldShowWhenJSON(t *testing.T) {
	f := Field{
		Name: "address", Label: "地址", Type: FieldString,
		ShowWhen: &ShowWhen{Field: "mode", In: []string{"tcp", "rtu-over-tcp"}},
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"showWhen"`) || !strings.Contains(s, `"field":"mode"`) {
		t.Fatalf("showWhen not serialized: %s", s)
	}
}

func TestListDriverWithoutSchema(t *testing.T) {
	Register("plaindriver", plainDriver{})

	found := false
	for _, info := range List() {
		if info.Name != "plaindriver" {
			continue
		}
		found = true
		if info.Config != nil || info.Params != nil {
			t.Fatalf("plaindriver should have nil schema, got %+v", info)
		}
	}
	if !found {
		t.Fatal("plaindriver not listed")
	}
}
