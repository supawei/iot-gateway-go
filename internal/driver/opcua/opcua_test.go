package opcua

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/gopcua/opcua/ua"

	"iot-gateway-go/internal/model"
)

func TestParseConnConfig(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantErr      bool
		wantEndpoint string
	}{
		{"valid", `{"endpoint":"opc.tcp://x:4840"}`, false, "opc.tcp://x:4840"},
		{"missing endpoint", `{}`, true, ""},
		{"unsupported security", `{"endpoint":"opc.tcp://x:4840","securityMode":"sign"}`, true, ""},
		{"invalid timeout", `{"endpoint":"opc.tcp://x:4840","timeout":"abc"}`, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseConnConfig(json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr && cfg.Endpoint != tt.wantEndpoint {
				t.Fatalf("endpoint=%q want %q", cfg.Endpoint, tt.wantEndpoint)
			}
		})
	}
}

func TestParseConnConfigDefaults(t *testing.T) {
	cfg, err := parseConnConfig(json.RawMessage(`{"endpoint":"opc.tcp://x:4840"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.SecurityMode != "none" || cfg.Timeout != "5s" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

// TestParseNodeIDAddress 验证点位地址的 NodeID 字符串约定可被 gopcua 正确解析。
// TestParseNodeIDAddress 验证点位地址的 NodeID 字符串约定可被 gopcua 正确解析。
// 注:ParseNodeID 较宽松(裸字符串视为 ns=0 的 string node),非法地址在运行时
// 由 server 返回 bad status 体现,而非解析期报错。
func TestParseNodeIDAddress(t *testing.T) {
	for _, addr := range []string{"ns=2;s=Temperature", "ns=0;i=2258", "i=1234", "s=Foo"} {
		t.Run(addr, func(t *testing.T) {
			nodeID, err := ua.ParseNodeID(addr)
			if err != nil || nodeID == nil {
				t.Fatalf("parse %q: err=%v nodeID=%v", addr, err, nodeID)
			}
		})
	}
}

func TestDecodeValue(t *testing.T) {
	if _, ok := decodeValue("x", model.DataTypeBool, 0); ok {
		t.Fatalf("string as bool should fail")
	}
	if _, ok := decodeValue("x", model.DataTypeDouble, 0); ok {
		t.Fatalf("string as double should fail")
	}

	got, ok := decodeValue(true, model.DataTypeBool, 0)
	if !ok || got != true {
		t.Fatalf("bool: got %v ok %v", got, ok)
	}

	got, ok = decodeValue("temp", model.DataTypeString, 0)
	if !ok || got != "temp" {
		t.Fatalf("string: got %v ok %v", got, ok)
	}

	got, ok = decodeValue(int32(42), model.DataTypeInt32, 0)
	if !ok || got != int64(42) {
		t.Fatalf("int32: got %v ok %v", got, ok)
	}

	got, ok = decodeValue(float64(3.14), model.DataTypeDouble, 0)
	if !ok {
		t.Fatalf("double ok=false")
	}
	if floatValue, _ := got.(float64); floatValue != 3.14 {
		t.Fatalf("double value: %v", got)
	}

	got, ok = decodeValue(int32(42), model.DataTypeInt32, 0.1)
	if !ok {
		t.Fatalf("int32 scaled ok=false")
	}
	if floatValue, _ := got.(float64); math.Abs(floatValue-4.2) > 1e-9 {
		t.Fatalf("int32 scaled value: %v want 4.2", got)
	}
}

func TestConnectionPoolAcquireRelease(t *testing.T) {
	d := &opcuaDriver{pool: make(map[string]*sharedSession)}
	shared := &sharedSession{connectionID: "c1", refCount: 1}
	d.pool["c1"] = shared

	got, err := d.acquire(context.Background(), "c1", connConfig{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got != shared || shared.refCount != 2 {
		t.Fatalf("acquire did not reuse: refCount=%d", shared.refCount)
	}

	if err := d.release(shared); err != nil {
		t.Fatalf("release: %v", err)
	}
	if shared.refCount != 1 {
		t.Fatalf("refCount=%d want 1", shared.refCount)
	}
}
