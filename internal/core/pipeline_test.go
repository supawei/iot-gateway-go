package core

import (
	"context"
	"testing"

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
	defer cancel()

	done := make(chan struct{})
	go func() {
		RunPipeline(ctx, dataPoints, []output.Output{mock})
		close(done)
	}()

	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p1", Value: 1, Quality: model.QualityGood}
	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p2", Value: 2, Quality: model.QualityGood}
	close(dataPoints) // 通知 pipeline 无更多数据,排空已入队数据后退出

	<-done // 等待 pipeline 处理完两个数据点并返回,建立 happens-before

	if len(mock.received) != 2 {
		t.Fatalf("want 2 datapoints got %d", len(mock.received))
	}
}
