package core

import (
	"context"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

type mockOutput struct {
	received []model.DataPoint
}

func (m *mockOutput) Publish(dp model.DataPoint) error {
	m.received = append(m.received, dp)
	return nil
}

func (m *mockOutput) Close() error { return nil }

func TestRunPipeline(t *testing.T) {
	dataPoints := make(chan model.DataPoint, 2)
	mock := &mockOutput{}
	ctx, cancel := context.WithCancel(context.Background())
	go RunPipeline(ctx, dataPoints, []output.Output{mock})

	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p1", Value: 1, Quality: model.QualityGood}
	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p2", Value: 2, Quality: model.QualityGood}

	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)

	if len(mock.received) != 2 {
		t.Fatalf("want 2 datapoints got %d", len(mock.received))
	}
}
