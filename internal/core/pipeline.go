package core

import (
	"context"
	"log/slog"
	"sync"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

// outputQueueSize 是每个北向输出各自的缓冲队列长度:队列满时丢弃新数据点并告警,
// 避免慢输出(如 MQTT 阻塞)反压回采集侧拖垮全局。
const outputQueueSize = 1024

// RunPipeline 消费采集到的数据点,分发给所有输出。
// 每个输出各自一个带缓冲队列 + 独立发布 goroutine,实现输出间解耦与背压隔离;
// 处理层(过滤/规则)未来在此处插入,目前直通。
func RunPipeline(ctx context.Context, dataPoints <-chan model.DataPoint, outputs []output.Output) {
	var wg sync.WaitGroup
	queues := make([]chan model.DataPoint, len(outputs))
	for i, out := range outputs {
		queues[i] = make(chan model.DataPoint, outputQueueSize)
		wg.Add(1)
		go func(ch <-chan model.DataPoint, o output.Output) {
			defer wg.Done()
			publishLoop(ctx, ch, o)
		}(queues[i], out)
	}
	for {
		select {
		case <-ctx.Done():
			wg.Wait() // 等所有发布 goroutine 退出后再返回,保证调用方 Close 输出安全
			return
		case dp, ok := <-dataPoints:
			if !ok {
				return
			}
			for _, q := range queues {
				select {
				case q <- dp:
				default:
					slog.Warn("output queue full, drop datapoint", "device", dp.DeviceID, "point", dp.Point)
				}
			}
		}
	}
}

// publishLoop 从单个输出的队列取数据点并发布;ctx 取消即退出。
func publishLoop(ctx context.Context, ch <-chan model.DataPoint, out output.Output) {
	for {
		select {
		case <-ctx.Done():
			return
		case dp := <-ch:
			if err := out.Publish(dp); err != nil {
				slog.Error("publish datapoint failed", "err", err)
			}
		}
	}
}
