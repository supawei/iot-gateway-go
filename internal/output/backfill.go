package output

import "iot-gateway-go/internal/model"

// 断网本地补传(离线缓存)相关的能力接口。
// 详见 docs/offline-backfill-design.md。

// BackfillSink 是输出把"无法即时送出的数据点"持久化到补传队列的能力。
// 由 main 注入(实现者为 *backfill.Store,经 BuildContext.Backfill 传入)。
// 输出在内存缓冲满、上送失败等丢点路径上调用 Save 而非丢弃,保证数据不丢。
type BackfillSink interface {
	Save(outputID string, dps []model.DataPoint) error
}

// BackfillHealthy 是输出的可选能力:报告当前是否处于"可正常上送"状态。
// Manager 只在 healthy 时对该输出的补传队列执行重放,避免把数据灌进仍不可用的连接。
// MQTT 型输出返回 client.IsConnected();无长连接的 HTTP 型输出按"最近上送无持续错误"判定。
type BackfillHealthy interface {
	BackfillHealthy() bool
}
