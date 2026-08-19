//go:build !linux

package observability

// 非 Linux 平台的降级实现:三项系统指标不暴露(Linux 网关目标平台走 sys_linux.go)。
// 保留接口签名让 metrics.go 无 build 约束地调用。

func processRSS() (int64, bool)                          { return 0, false }
func systemMemUsedPercent() (float64, bool)              { return 0, false }
func diskUsedPercent(paths ...string) map[string]float64 { return nil }
