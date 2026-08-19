package observability

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"iot-gateway-go/internal/core"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/processing"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/version"
)

// Collector 聚合网关运行时指标,渲染为 Prometheus text exposition 供 /metrics 暴露。
// 抓取时即时读内存态快照,无缓存、无持久化(沿用 output-status-design 的可观测边界)。
// 各数据源指针可为 nil(测试用),writeMetrics 跳过 nil 来源的指标族。
type Collector struct {
	status    *status.Registry
	outputs   *output.Manager
	proc      *processing.Engine
	scheduler *core.Scheduler
	startTime time.Time
	dataPath  string // SQLite 文件所在目录(磁盘指标)
	logPath   string // 日志文件所在目录(磁盘指标),空则只统计数据目录
}

// NewCollector 构造采集器。dataPath/logPath 为磁盘占用率统计的分区路径。
func NewCollector(statusReg *status.Registry, outputs *output.Manager, proc *processing.Engine, sched *core.Scheduler, dataPath, logPath string) *Collector {
	return &Collector{
		status:    statusReg,
		outputs:   outputs,
		proc:      proc,
		scheduler: sched,
		startTime: time.Now(),
		dataPath:  dataPath,
		logPath:   logPath,
	}
}

// MetricsHandler 暴露 Prometheus text exposition 格式指标,匿名无鉴权。
func (c *Collector) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	c.writeMetrics(&b)
	_, _ = io.WriteString(w, b.String())
}

func (c *Collector) writeMetrics(b *strings.Builder) {
	c.writeRuntimeMetrics(b)
	if c.scheduler != nil {
		c.writeSchedulerMetrics(b)
	}
	if c.status != nil {
		c.writeDeviceMetrics(b)
	}
	if c.proc != nil {
		c.writeProcessingMetrics(b)
	}
	if c.outputs != nil {
		c.writeOutputMetrics(b, c.outputs.Status())
	}
}

func (c *Collector) writeRuntimeMetrics(b *strings.Builder) {
	metric(b, "gauge", "iot_gateway_uptime_seconds", "Gateway process uptime in seconds.")
	sample(b, "iot_gateway_uptime_seconds", "", time.Since(c.startTime).Seconds())
	metric(b, "gauge", "iot_gateway_info", "Gateway build info; the value is always 1.")
	sample(b, "iot_gateway_info", labels("version", version.Version, "commit", version.Commit), 1)
	metric(b, "gauge", "iot_gateway_go_goroutines", "Number of goroutines.")
	sample(b, "iot_gateway_go_goroutines", "", float64(runtime.NumGoroutine()))

	if rss, ok := processRSS(); ok {
		metric(b, "gauge", "iot_gateway_process_rss_bytes", "Gateway process resident set size in bytes.")
		sample(b, "iot_gateway_process_rss_bytes", "", float64(rss))
	}
	if used, ok := systemMemUsedPercent(); ok {
		metric(b, "gauge", "iot_gateway_mem_used_percent", "System memory used percent.")
		sample(b, "iot_gateway_mem_used_percent", "", used)
	}
	if paths := diskUsedPercent(c.dataPath, c.logPath); len(paths) > 0 {
		metric(b, "gauge", "iot_gateway_disk_used_percent", "Filesystem used percent of data/log partitions.")
		for _, p := range sortedKeys(paths) {
			sample(b, "iot_gateway_disk_used_percent", labels("path", p), paths[p])
		}
	}
}

func (c *Collector) writeSchedulerMetrics(b *strings.Builder) {
	s := c.scheduler.Stats()
	metric(b, "counter", "iot_gateway_collect_total", "Total number of poll collect executions.")
	sample(b, "iot_gateway_collect_total", "", float64(s.CollectTotal))
	metric(b, "counter", "iot_gateway_collect_errors_total", "Total number of failed poll collects.")
	sample(b, "iot_gateway_collect_errors_total", "", float64(s.CollectErrors))
	metric(b, "gauge", "iot_gateway_task_queue_length", "Scheduler task queue current length.")
	sample(b, "iot_gateway_task_queue_length", "", float64(s.TaskQueueLen))
	metric(b, "gauge", "iot_gateway_task_queue_capacity", "Scheduler task queue capacity (2x pool size).")
	sample(b, "iot_gateway_task_queue_capacity", "", float64(s.TaskQueueCap))
}

func (c *Collector) writeDeviceMetrics(b *strings.Builder) {
	online, total := 0, 0
	for _, d := range c.status.List() {
		total++
		if d.Online {
			online++
		}
	}
	metric(b, "gauge", "iot_gateway_devices_online", "Number of online devices.")
	sample(b, "iot_gateway_devices_online", "", float64(online))
	metric(b, "gauge", "iot_gateway_devices_total", "Number of devices the scheduler has registered status for.")
	sample(b, "iot_gateway_devices_total", "", float64(total))
}

func (c *Collector) writeProcessingMetrics(b *strings.Builder) {
	ps := c.proc.Stats()
	metric(b, "counter", "iot_gateway_processing_points_in_total", "Total datapoints entering processing.")
	sample(b, "iot_gateway_processing_points_in_total", "", float64(ps.PointsIn))
	metric(b, "counter", "iot_gateway_processing_points_passed_total", "Total datapoints passed through processing.")
	sample(b, "iot_gateway_processing_points_passed_total", "", float64(ps.PointsPass))
	metric(b, "counter", "iot_gateway_processing_points_filtered_total", "Total datapoints filtered out by processing.")
	sample(b, "iot_gateway_processing_points_filtered_total", "", float64(ps.PointsFiltered))
	metric(b, "counter", "iot_gateway_processing_points_aggregated_total", "Total datapoints emitted by aggregators.")
	sample(b, "iot_gateway_processing_points_aggregated_total", "", float64(ps.PointsAggregated))
}

func (c *Collector) writeOutputMetrics(b *strings.Builder, outs []output.OutputStatus) {
	outputFamily(b, "gauge", "iot_gateway_output_connected", "Whether the output logical connection is established (1/0).",
		outs, func(o output.OutputStatus) float64 { return boolToFloat(o.Connected) })
	outputFamily(b, "counter", "iot_gateway_output_sent_total", "Total datapoints successfully sent per output.",
		outs, func(o output.OutputStatus) float64 { return float64(o.Sent) })
	outputFamily(b, "gauge", "iot_gateway_output_pending", "Output internal pending buffer count.",
		outs, func(o output.OutputStatus) float64 { return float64(o.Pending) })
	outputFamily(b, "counter", "iot_gateway_output_dropped_total", "Total datapoints dropped per output (queue full, no backfill).",
		outs, func(o output.OutputStatus) float64 { return float64(o.Dropped) })
	outputFamily(b, "gauge", "iot_gateway_output_queue_used", "Output fan-out queue used slots.",
		outs, func(o output.OutputStatus) float64 { return float64(o.QueueUsed) })
	outputFamily(b, "gauge", "iot_gateway_output_queue_capacity", "Output fan-out queue capacity.",
		outs, func(o output.OutputStatus) float64 { return float64(o.QueueCap) })
	outputFamily(b, "gauge", "iot_gateway_output_backlog", "Backfill queue depth per output.",
		outs, func(o output.OutputStatus) float64 { return float64(o.Backfill) })
}

// outputFamily 输出一个指标族:HELP/TYPE 各一次,随后每个输出一条带 output_id 标签的样本。
func outputFamily(b *strings.Builder, mtype, name, help string, outs []output.OutputStatus, val func(output.OutputStatus) float64) {
	metric(b, mtype, name, help)
	for _, o := range outs {
		sample(b, name, labels("output_id", o.OutputID), val(o))
	}
}

// metric 写 HELP 与 TYPE 行(每个指标名一次)。
func metric(b *strings.Builder, mtype, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, mtype)
}

// sample 写一条样本:iot_gateway_xxx{labels} value 或无标签时 iot_gateway_xxx value。
func sample(b *strings.Builder, name, labelsStr string, value float64) {
	if labelsStr != "" {
		fmt.Fprintf(b, "%s{%s} %s\n", name, labelsStr, formatFloat(value))
		return
	}
	fmt.Fprintf(b, "%s %s\n", name, formatFloat(value))
}

// labels 把键值对拼成 k="v",k2="v2";值经 escapeLabel 转义。
func labels(kv ...string) string {
	var sb strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%s=\"%s\"", kv[i], escapeLabel(kv[i+1]))
	}
	return sb.String()
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// sortedKeys 返回 map 的键排序,保证指标输出稳定(便于测试快照与人类阅读)。
func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 简单插入排序:disk 路径数极少(1~2),不必引 sort。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
