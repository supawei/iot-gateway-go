package output

import (
	"fmt"
	"log/slog"

	"iot-gateway-go/internal/model"
)

// BuildSet 逐个构建一组输出;单个输出构建失败只跳过并告警,不拖垮其余(失败隔离)。
// 返回 (outputs, nil) 表示至少一个输出构建成功(失败项已跳过);
// 返回 (nil, err) 表示存在已启用的输出但全部构建失败,调用方应保留旧输出。
// 设计见 docs/mqtt-resilience-design.md §4.3。
func BuildSet(bc BuildContext, configs []model.Output) ([]Output, error) {
	result := make([]Output, 0, len(configs))
	enabled := 0
	for _, o := range configs {
		if !o.Enabled {
			continue
		}
		enabled++
		out, err := Build(bc, o.Type, o.Config)
		if err != nil {
			slog.Error("build output failed, skipped", "id", o.ID, "type", o.Type, "err", err)
			continue
		}
		result = append(result, out)
	}
	if enabled > 0 && len(result) == 0 {
		return nil, fmt.Errorf("all %d enabled outputs failed to build", enabled)
	}
	return result, nil
}
