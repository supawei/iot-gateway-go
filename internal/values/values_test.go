package values

import (
	"testing"
	"time"

	"iot-gateway-go/internal/model"
)

func TestRegistryGetSorted(t *testing.T) {
	r := NewRegistry()
	ts := time.Now()

	r.Update(model.DataPoint{DeviceID: "d1", Point: "b", Value: 2, Quality: model.QualityGood, Timestamp: ts})
	r.Update(model.DataPoint{DeviceID: "d1", Point: "a", Value: 1, Quality: model.QualityGood, Timestamp: ts})
	r.Update(model.DataPoint{DeviceID: "d1", Point: "a", Value: 9, Quality: model.QualityGood, Timestamp: ts})

	got := r.Get("d1")
	if got.DeviceID != "d1" || len(got.Points) != 2 {
		t.Fatalf("get: %+v", got)
	}
	if got.Points[0].Point != "a" || got.Points[1].Point != "b" {
		t.Fatalf("not sorted: %+v", got.Points)
	}
	if got.Points[0].Value != 9 {
		t.Fatalf("latest value not kept: %+v", got.Points[0])
	}
}

func TestRegistryEmptyAndBadQuality(t *testing.T) {
	r := NewRegistry()

	if got := r.Get("nonexistent"); got.DeviceID != "nonexistent" || len(got.Points) != 0 {
		t.Fatalf("empty get: %+v", got)
	}

	// bad 质量点也记录,值为 nil,界面据此展示质量状态。
	r.Update(model.DataPoint{DeviceID: "d1", Point: "p", Quality: model.QualityBad})
	got := r.Get("d1")
	if len(got.Points) != 1 || got.Points[0].Value != nil || got.Points[0].Quality != model.QualityBad {
		t.Fatalf("bad quality not recorded: %+v", got)
	}
}
