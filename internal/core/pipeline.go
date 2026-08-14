package core

import (
	"context"
	"log/slog"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

// RunPipeline 消费采集到的数据点,分发给所有输出。
// 处理层(过滤/规则)未来在此处插入,目前直通。
func RunPipeline(ctx context.Context, dataPoints <-chan model.DataPoint, outputs []output.Output) {
	for {
		select {
		case <-ctx.Done():
			return
		case dp, ok := <-dataPoints:
			if !ok {
				return
			}
			publishToAll(dp, outputs)
		}
	}
}

func publishToAll(dp model.DataPoint, outputs []output.Output) {
	for _, out := range outputs {
		if err := out.Publish(dp); err != nil {
			slog.Error("publish datapoint failed", "err", err)
		}
	}
}
