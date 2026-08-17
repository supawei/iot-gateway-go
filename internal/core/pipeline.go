package core

import (
	"context"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

// RunPipeline 消费采集到的数据点,分发给当前活跃输出。
// 扇出(每个输出独立队列 + 发布 goroutine、背压隔离)与热重载由 output.Manager 负责;
// 处理层(过滤/规则)未来在此处插入,目前直通。
func RunPipeline(ctx context.Context, dataPoints <-chan model.DataPoint, mgr *output.Manager) {
	for {
		select {
		case <-ctx.Done():
			return
		case dp, ok := <-dataPoints:
			if !ok {
				return
			}
			mgr.Publish(dp)
		}
	}
}
