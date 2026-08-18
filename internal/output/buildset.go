package output

import (
	"fmt"
	"log/slog"

	"iot-gateway-go/internal/model"
)

// BuildSet 逐个构建一组输出;单个输出构建失败只跳过并告警,不拖垮其余(失败隔离)。
// 返回的 Instance 携带配置身份(ID/Name/Type/Enabled),供 Manager 关联运行态。
// 返回 (instances, nil) 表示至少一个输出构建成功(失败项已跳过);
// 返回 (nil, err) 表示存在已启用的输出但全部构建失败,调用方应保留旧输出。
// 设计见 docs/mqtt-resilience-design.md §4.3。
func BuildSet(bc BuildContext, configs []model.Output) ([]Instance, error) {
	result := make([]Instance, 0, len(configs))
	enabled := 0
	for _, o := range configs {
		if !o.Enabled {
			continue
		}
		enabled++
		// 逐条注入当前实例的配置 ID:输出用它作为补传队列(output_id)的隔离键。
		bc.OutputID = o.ID
		out, err := Build(bc, o.Type, o.Config)
		if err != nil {
			slog.Error("build output failed, skipped", "id", o.ID, "type", o.Type, "err", err)
			continue
		}
		result = append(result, Instance{Out: out, ID: o.ID, Name: o.Name, Type: o.Type, Enabled: o.Enabled})
	}
	if enabled > 0 && len(result) == 0 {
		return nil, fmt.Errorf("all %d enabled outputs failed to build", enabled)
	}
	return result, nil
}
