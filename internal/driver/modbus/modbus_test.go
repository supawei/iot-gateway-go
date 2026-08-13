package modbus

import (
	"encoding/json"
	"testing"

	"iot-gateway-go/internal/model"
)

func TestParseAddress(t *testing.T) {
	tests := []struct {
		addr     string
		function string
		register uint16
		wantErr  bool
	}{
		{"holding:0", "holding", 0, false},
		{"coil:2", "coil", 2, false},
		{"input:65535", "input", 65535, false},
		{"holding", "", 0, true},
		{"holding:-1", "", 0, true},
		{"holding:65536", "", 0, true},
	}
	for _, tc := range tests {
		function, register, err := parseAddress(tc.addr)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseAddress(%q) err=%v wantErr=%v", tc.addr, err, tc.wantErr)
		}
		if !tc.wantErr && (function != tc.function || register != tc.register) {
			t.Fatalf("parseAddress(%q)=%q,%d want %q,%d", tc.addr, function, register, tc.function, tc.register)
		}
	}
}

func TestQuantityOf(t *testing.T) {
	tests := map[model.DataType]uint16{
		model.DataTypeBool:   1,
		model.DataTypeInt16:  1,
		model.DataTypeUInt16: 1,
		model.DataTypeInt32:  2,
		model.DataTypeUInt32: 2,
		model.DataTypeFloat:  2,
	}
	for dataType, want := range tests {
		if got := quantityOf(dataType); got != want {
			t.Fatalf("quantityOf(%s)=%d want %d", dataType, got, want)
		}
	}
}

func TestDecodeValue(t *testing.T) {
	if v, err := decodeValue(model.DataTypeBool, []byte{1}); err != nil || v != true {
		t.Fatalf("bool decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeInt16, []byte{0x01, 0x02}); err != nil || v != int16(258) {
		t.Fatalf("int16 decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeUInt16, []byte{0x01, 0x02}); err != nil || v != uint16(258) {
		t.Fatalf("uint16 decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeInt32, []byte{0x00, 0x00, 0x01, 0x02}); err != nil || v != int32(258) {
		t.Fatalf("int32 decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeFloat, []byte{0x43, 0x7A, 0x00, 0x00}); err != nil || v != float32(250.0) {
		t.Fatalf("float decode: %v err=%v", v, err)
	}
	if _, err := decodeValue(model.DataTypeInt16, []byte{0x01}); err == nil {
		t.Fatal("expected error for short response")
	}
}

func TestApplyScale(t *testing.T) {
	if v := applyScale(int16(5), 2.0, model.DataTypeInt16); v != 10.0 {
		t.Fatalf("scale int16: %v want 10.0", v)
	}
	if v := applyScale(uint16(3), 2.0, model.DataTypeUInt16); v != 6.0 {
		t.Fatalf("scale uint16: %v want 6.0", v)
	}
	if v := applyScale(true, 0.1, model.DataTypeBool); v != true {
		t.Fatalf("scale bool should be unchanged: %v", v)
	}
	if v := applyScale(int16(5), 0, model.DataTypeInt16); v != int16(5) {
		t.Fatalf("scale 0 should be unchanged: %v", v)
	}
}

func TestParseConnConfig(t *testing.T) {
	cfg, err := parseConnConfig(json.RawMessage(`{"mode":"tcp","address":"192.168.1.5:502","slaveId":1}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.Mode != "tcp" || cfg.Address != "192.168.1.5:502" || cfg.SlaveID != 1 {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.BaudRate != defaultBaudRate {
		t.Fatalf("default baud rate: %d want %d", cfg.BaudRate, defaultBaudRate)
	}
	rtuOverTCP, err := parseConnConfig(json.RawMessage(`{"mode":"rtu-over-tcp","address":"192.168.1.5:502","slaveId":2}`))
	if err != nil {
		t.Fatalf("rtu-over-tcp err: %v", err)
	}
	if rtuOverTCP.Mode != "rtu-over-tcp" || rtuOverTCP.Address != "192.168.1.5:502" || rtuOverTCP.SlaveID != 2 {
		t.Fatalf("unexpected rtu-over-tcp cfg: %+v", rtuOverTCP)
	}
	if _, err := parseConnConfig(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for missing mode")
	}
}
