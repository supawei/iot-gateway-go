package core

import (
	"context"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/processing"
	"iot-gateway-go/internal/store"
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

// newTestManager 用一组输出构造 Manager 并完成首次 Reload。
func newTestManager(t *testing.T, outs ...output.Output) *output.Manager {
	t.Helper()
	insts := make([]output.Instance, 0, len(outs))
	for _, o := range outs {
		insts = append(insts, output.Instance{Out: o})
	}
	mgr := output.NewManager(func() ([]output.Instance, error) {
		return insts, nil
	})
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload outputs: %v", err)
	}
	t.Cleanup(mgr.Close)
	return mgr
}

// newTestEngine 构造直通处理引擎(内存 store,无任何点位处理规则,全部直通)。
func newTestEngine(t *testing.T, out func(model.DataPoint)) *processing.Engine {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return processing.NewEngine(st, out)
}

func TestRunPipeline(t *testing.T) {
	dataPoints := make(chan model.DataPoint, 2)
	mock := &mockOutput{ch: make(chan model.DataPoint, 4)}
	mgr := newTestManager(t, mock)
	proc := newTestEngine(t, mgr.Publish)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunPipeline(ctx, dataPoints, proc, mgr)

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
	mgr := newTestManager(t, out1, out2)
	proc := newTestEngine(t, mgr.Publish)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunPipeline(ctx, dataPoints, proc, mgr)

	dataPoints <- model.DataPoint{DeviceID: "d1", Point: "p1", Value: 1, Quality: model.QualityGood}
	for _, ch := range []chan model.DataPoint{out1.ch, out2.ch} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for fanout datapoint")
		}
	}
}
