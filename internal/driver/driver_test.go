package driver

import (
	"context"
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
