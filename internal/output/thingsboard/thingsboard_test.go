package thingsboard

import (
	"encoding/json"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
)

func TestDeviceName(t *testing.T) {
	withPrefix := &thingsboardOutput{prefix: "factory1/"}
	if got := withPrefix.deviceName("sensor-01"); got != "factory1/sensor-01" {
		t.Fatalf("deviceName with prefix = %q", got)
	}
	noPrefix := &thingsboardOutput{}
	if got := noPrefix.deviceName("sensor-01"); got != "sensor-01" {
		t.Fatalf("deviceName without prefix = %q", got)
	}
}

func TestTelemetryBatchPayloadSinglePoint(t *testing.T) {
	dp := model.DataPoint{
		DeviceID:  "sensor-01",
		Point:     "temperature",
		Value:     25.5,
		Timestamp: time.UnixMilli(1700000000000),
		Quality:   model.QualityGood,
	}
	got, err := json.Marshal(telemetryBatchPayload("sensor-01", []model.DataPoint{dp}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sensor-01":[{"ts":1700000000000,"values":{"temperature":25.5}}]}`
	if string(got) != want {
		t.Fatalf("payload:\n got %s\nwant %s", got, want)
	}
}

// TestTelemetryBatchPayloadGroupsByTimestamp 验证同一设备多时刻的点位按时间戳分组,
// 同一时刻的多点位合并进一个 values。
func TestTelemetryBatchPayloadGroupsByTimestamp(t *testing.T) {
	ts := time.UnixMilli(1700000000000)
	points := []model.DataPoint{
		{Point: "temperature", Value: 25.5, Timestamp: ts},
		{Point: "humidity", Value: 60, Timestamp: ts},
		{Point: "temperature", Value: 26.0, Timestamp: ts.Add(time.Second)},
	}
	got, err := json.Marshal(telemetryBatchPayload("sensor-01", points))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sensor-01":[{"ts":1700000000000,"values":{"humidity":60,"temperature":25.5}},{"ts":1700000001000,"values":{"temperature":26}}]}`
	if string(got) != want {
		t.Fatalf("payload:\n got %s\nwant %s", got, want)
	}
}

func TestAttributesPayload(t *testing.T) {
	got, err := json.Marshal(attributesPayload("sensor-01", map[string]interface{}{"quality": "good"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sensor-01":{"quality":"good"}}`
	if string(got) != want {
		t.Fatalf("attributes payload:\n got %s\nwant %s", got, want)
	}
}

// TestDeviceNotifierIntents 验证 DeviceOnline/DeviceOffline 记录 connect/disconnect 意图,后者覆盖前者。
func TestDeviceNotifierIntents(t *testing.T) {
	o := &thingsboardOutput{
		connects:    make(map[string]bool),
		disconnects: make(map[string]bool),
	}
	o.DeviceOnline("sensor-01")
	if !o.connects["sensor-01"] {
		t.Fatal("online should record connect intent")
	}
	o.DeviceOffline("sensor-01")
	if !o.disconnects["sensor-01"] || o.connects["sensor-01"] {
		t.Fatal("offline should override online intent")
	}
}
