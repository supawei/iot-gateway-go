package main

import (
	"strconv"
	"strings"
)

// metrics 是 /metrics 文本的一次快照解析结果。
// key 为"指标名"或"指标名{标签}"(原样),value 为该行数值。
type metrics struct {
	values map[string]float64
}

// get 取指标值;不存在返回 0。
func (m *metrics) get(name string) float64 {
	if v, ok := m.values[name]; ok {
		return v
	}
	return 0
}

// parseMetrics 解析 Prometheus text exposition 的裸数值行(name value 或 name{labels} value)。
// 忽略 # HELP/# TYPE 注释与不以数字结尾的行;同一 key 多行(多标签组合)取总和。
func parseMetrics(body string) *metrics {
	m := &metrics{values: make(map[string]float64)}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sp := strings.LastIndex(line, " ")
		if sp <= 0 {
			continue
		}
		name := line[:sp]
		valStr := line[sp+1:]
		// 允许尾部换行等;值必须是数字
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		// 去掉标签,取"指标名"维度的合计(压测关注总量/速率,不细分标签)
		base := name
		if i := strings.Index(name, "{"); i > 0 {
			base = name[:i]
		}
		m.values[base] += v
	}
	return m
}
