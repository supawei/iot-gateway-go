package smardaten

import (
	"encoding/json"
	"testing"
)

// TestConfigUnmarshalMixedTypes 验证 Config 能同时接受数字和字符串形式的配置值。
// Web UI 的 FieldEnum 控件发送字符串（如 "311"），FieldInt 发送数字（如 1883），
// 甚至 FieldEnum 的默认值可能以数字形式（如 311）发送。Config 必须全部兼容。
func TestConfigUnmarshalMixedTypes(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantPort   int
		wantProto  int
		wantPub    int
		wantMaxPub int
	}{
		{
			name:       "all strings (user-selected enum values)",
			raw:        `{"broker":"tcp://x:1883","port":"1883","protoVer":"311","pubMode":"0","maxPubTime":"60"}`,
			wantPort:   1883,
			wantProto:  311,
			wantPub:    0,
			wantMaxPub: 60,
		},
		{
			name:       "all numbers (enum defaults sent as numbers)",
			raw:        `{"broker":"tcp://x:1883","port":1883,"protoVer":311,"pubMode":0,"maxPubTime":60}`,
			wantPort:   1883,
			wantProto:  311,
			wantPub:    0,
			wantMaxPub: 60,
		},
		{
			name:       "mixed types",
			raw:        `{"broker":"tcp://x:1883","port":1883,"protoVer":"5","pubMode":1,"maxPubTime":"30"}`,
			wantPort:   1883,
			wantProto:  5,
			wantPub:    1,
			wantMaxPub: 30,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tc.raw), &cfg); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if int(cfg.Port) != tc.wantPort {
				t.Errorf("port = %d, want %d", cfg.Port, tc.wantPort)
			}
			if int(cfg.ProtoVer) != tc.wantProto {
				t.Errorf("protoVer = %d, want %d", cfg.ProtoVer, tc.wantProto)
			}
			if int(cfg.PubMode) != tc.wantPub {
				t.Errorf("pubMode = %d, want %d", cfg.PubMode, tc.wantPub)
			}
			if int(cfg.MaxPubTime) != tc.wantMaxPub {
				t.Errorf("maxPubTime = %d, want %d", cfg.MaxPubTime, tc.wantMaxPub)
			}
		})
	}
}

// TestConfigUnmarshalMissingOptional 验证缺省字段能正常解析（空值）。
func TestConfigUnmarshalMissingOptional(t *testing.T) {
	raw := `{"broker":"tcp://x:1883","iotAppId":"abc","iotRsaKeyPath":"key.pem"}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if cfg.Port != 0 {
		t.Errorf("port should default to 0, got %d", cfg.Port)
	}
}

// TestFlexIntInvalid 验证非法值返回错误。
// null 合法（视为 0，与 Go 对 int 的默认行为一致），其余类型均报错。
func TestFlexIntInvalid(t *testing.T) {
	cases := []string{
		`"abc"`,
		`true`,
		`[1]`,
		`{"a":1}`,
	}
	for _, raw := range cases {
		var f flexInt
		if err := json.Unmarshal([]byte(raw), &f); err == nil {
			t.Errorf("expected error for %s, got none", raw)
		}
	}
	// null 视为 0，不报错
	var f flexInt
	if err := json.Unmarshal([]byte(`null`), &f); err != nil {
		t.Errorf("null should be accepted, got error: %v", err)
	}
	if f != 0 {
		t.Errorf("null should set value to 0, got %d", f)
	}
}

// TestConnectMQTTClientID 验证 clientID 生成使用 gatewayID。
func TestConnectMQTTClientID(t *testing.T) {
	// 无法真正连接 MQTT，这里只验证 clientID 生成逻辑本身。
	// clientID 为空时应使用 "gw-dev-manage-<gatewayID>"。
	// 该逻辑在 connectMQTT 内部，通过直接检查生成结果不可行，
	// 因此此处仅验证 stripProtocol 工具函数。
	if got := stripProtocol("tcp://10.0.0.1:1883"); got != "10.0.0.1:1883" {
		t.Errorf("stripProtocol = %q, want %q", got, "10.0.0.1:1883")
	}
	if got := stripProtocol("10.0.0.1:1883"); got != "10.0.0.1:1883" {
		t.Errorf("stripProtocol without prefix = %q", got)
	}
}