package core

import (
	"context"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/processing"
)

// RunPipeline 消费采集到的数据点,先经边缘处理层(过滤/聚合),再分发给当前活跃输出。
// 扇出(每个输出独立队列 + 发布 goroutine、背压隔离)与热重载由 output.Manager 负责;
// 过滤/聚合由 processing.Engine 承担,无处理规则的点直通。见 docs/edge-computing-design.md。
func RunPipeline(ctx context.Context, dataPoints <-chan model.DataPoint, proc *processing.Engine, mgr *output.Manager) {
	for {
		select {
		case <-ctx.Done():
			return
		case dp, ok := <-dataPoints:
			if !ok {
				return
			}
			proc.Process(dp)
		}
	}
}
