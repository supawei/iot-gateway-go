package core

import (
	"context"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

// mockOutput 用 channel 收集发布的数据点,适配 pipeline 的异步分片发布。
type mockOutput struct {
	ch chan model.DataPoint
}

func (m *mockOutput) Publish(dp model.DataPoint) error {
	m.ch <- dp
	return nil
}

func (m *mockOutput) Close() error { return nil }

func TestRunPipeline(t *testing.T) {
	dataPoints := make(chan model.DataPoint, 2)
	mock := &mockOutput{ch: make(chan model.DataPoint, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunPipeline(ctx, dataPoints, []output.Output{mock})

	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p1", Value: 1, Quality: model.QualityGood}
	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p2", Value: 2, Quality: model.QualityGood}

	// 异步发布:从 output 侧收 2 个,验证全部到达
	for i := 0; i < 2; i++ {
		select {
		case <-mock.ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for datapoint %d", i)
		}
	}
}

// TestRunPipelineFanout 验证一个数据点广播到多个输出。
func TestRunPipelineFanout(t *testing.T) {
	dataPoints := make(chan model.DataPoint, 1)
	out1 := &mockOutput{ch: make(chan model.DataPoint, 4)}
	out2 := &mockOutput{ch: make(chan model.DataPoint, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunPipeline(ctx, dataPoints, []output.Output{out1, out2})

	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p1", Value: 1, Quality: model.QualityGood}
	for _, ch := range []chan model.DataPoint{out1.ch, out2.ch} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for fanout datapoint")
		}
	}
}
