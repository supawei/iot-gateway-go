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

func TestTelemetryPayload(t *testing.T) {
	dp := model.DataPoint{
		DeviceID:  "sensor-01",
		Point:     "temperature",
		Value:     25.5,
		Timestamp: time.UnixMilli(1700000000000),
		Quality:   model.QualityGood,
	}
	got, err := json.Marshal(telemetryPayload("sensor-01", dp))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sensor-01":[{"ts":1700000000000,"values":{"temperature":25.5}}]}`
	if string(got) != want {
		t.Fatalf("telemetry payload:\n got %s\nwant %s", got, want)
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
