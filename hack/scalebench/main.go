// scalebench 是 iot-gateway-go 的规模化压测 harness。
//
// 用法(在仓库根目录):
//
//	go run ./hack/scalebench -devices 500 -points 4 -mqtt -duration 30s
//
// 流程:启动 Modbus TCP 模拟从站 + 记录型 MQTT 假 broker → 以子进程启动真实网关
// (auth 关闭)→ 经 REST 批量配置连接/设备/点位(可选 MQTT 输出与告警规则)→ 预热 →
// 在采样窗口内轮询 /metrics 与运行态 → 汇总采集/发布速率、内存、队列等指标。
//
// 详细方法与场景见 docs/scale-testing.md。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output/mqtttest"
)

// ---- 参数 ----

var (
	flagDevices  = flag.Int("devices", 500, "设备总数")
	flagPoints   = flag.Int("points", 4, "每设备点位数量")
	flagConns    = flag.Int("conns", 5, "连接(Modbus 从站传输)数量,设备轮询分布其上")
	flagInterval = flag.Int("interval-ms", 1000, "轮询周期(毫秒)")
	flagPool     = flag.Int("pool", 16, "scheduler worker pool 大小")
	flagMQTT     = flag.Bool("mqtt", false, "启用 MQTT 输出(发布到记录型假 broker)")
	flagAlerts   = flag.Int("alerts", 0, "附加的告警规则数量(引用点位,阈值不触发)")
	flagWarmup   = flag.Duration("warmup", 10*time.Second, "预热时长(等待连接建立与首轮采集)")
	flagDuration = flag.Duration("duration", 30*time.Second, "采样窗口时长")
	flagStep     = flag.Duration("step", 2*time.Second, "采样间隔")
	flagGateway  = flag.String("gateway", "", "网关二进制路径;为空时自动 go build 到临时目录")
	flagOut      = flag.String("out", "", "可选:CSV 汇总报告输出路径")
)

// sample 是采样窗口内的一次指标快照。
type sample struct {
	t        time.Time
	collect  float64 // 采集执行累计
	errs     float64 // 采集错误累计
	online   int     // 在线设备数
	rssMB    float64
	goros    float64
	memUsed  float64 // %
	queueLen float64
	queueCap float64
	sinkMsgs int64 // 假 broker 收到的发布数
	simReqs  int64 // 模拟从站收到的请求数
	simReads int64
	outSent  float64
	outDrop  float64
	outPend  float64
	procIn   float64
	procPass float64
	procFilt float64
	procAggr float64
}

type runEnv struct {
	tmpDir   string
	gwCmd    *exec.Cmd
	gwCancel context.CancelFunc
	sims     []*modbusSim // 每条连接一个独立模拟从站(端点防冲突:同 host:port 不可复用)
	sink     *mqtttest.RecordingBroker
	gwAddr   string // http://127.0.0.1:port
	apiBase  string // http://127.0.0.1:port/api/v1
}

func main() {
	flag.Parse()
	if *flagDevices <= 0 || *flagPoints <= 0 || *flagConns <= 0 {
		log.Fatal("devices/points/conns 必须为正数")
	}
	if *flagDuration <= 0 {
		log.Fatal("duration 必须为正")
	}

	env, err := setup()
	if err != nil {
		log.Fatalf("setup: %v", err)
	}
	defer env.cleanup()
	log.Printf("gateway ready at %s (sim %s, sink %s)", env.gwAddr, env.sims[0].Addr(), env.sink.Addr)

	if err := env.configure(); err != nil {
		log.Fatalf("configure: %v", err)
	}
	if err := env.warmup(); err != nil {
		log.Fatalf("warmup: %v", err)
	}
	samples := env.collect()
	report(samples)
}

// setup 启动模拟从站、假 broker 与网关子进程。
func setup() (*runEnv, error) {
	env := &runEnv{}
	var err error

	env.tmpDir, err = os.MkdirTemp("", "scalebench-")
	if err != nil {
		return nil, err
	}
	// 每条连接一个独立模拟从站(端点防冲突:同一 host:port 只能建一条连接)。
	for i := 0; i < *flagConns; i++ {
		sim, err := startModbusSim()
		if err != nil {
			return nil, fmt.Errorf("modbus sim %d: %w", i, err)
		}
		env.sims = append(env.sims, sim)
	}
	env.sink = mqtttest.StartRecordingStandalone()

	// 网关二进制:优先用指定路径,否则自动构建到临时目录。
	gwPath := *flagGateway
	if gwPath == "" {
		gwPath = filepath.Join(env.tmpDir, "gateway")
		if err := buildGateway(gwPath); err != nil {
			return nil, err
		}
	}

	gwPort := freePort()
	env.gwAddr = "http://127.0.0.1:" + gwPort
	env.apiBase = env.gwAddr + "/api/v1"

	cfg := fmt.Sprintf(`http:
  addr: "127.0.0.1:%s"
auth:
  enabled: false
storage:
  sqlitePath: "%s"
scheduler:
  poolSize: %d
log:
  level: "warn"
  file:
    path: "%s"
`, gwPort, filepath.Join(env.tmpDir, "gateway.db"), *flagPool, filepath.Join(env.tmpDir, "gateway.log"))
	cfgPath := filepath.Join(env.tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		return nil, err
	}

	logFile, err := os.Create(filepath.Join(env.tmpDir, "gw-stdout.log"))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, gwPath, cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		cancel()
		logFile.Close()
		return nil, fmt.Errorf("start gateway: %w", err)
	}
	env.gwCmd = cmd
	env.gwCancel = cancel

	// 等待就绪
	if err := waitReady(env.gwAddr, 20*time.Second); err != nil {
		return nil, err
	}
	return env, nil
}

// buildGateway 用仓库源码构建网关二进制。
func buildGateway(dst string) error {
	log.Printf("building gateway to %s ...", dst)
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/gateway")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build gateway: %w", err)
	}
	return nil
}

// waitReady 轮询 /readyz 直到 200。
func waitReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(addr + "/readyz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("gateway not ready within %v", timeout)
}

// freePort 返回一个空闲端口(短暂占用后释放,压测场景可接受)。
func freePort() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "18080"
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	return port
}

// post 发一个 REST JSON 请求(鉴权关闭直通)。返回是否 2xx。
func (e *runEnv) post(path string, body any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, e.apiBase+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// httpClient 带超时,避免网关异常时压测 harness 无限挂起。
var httpClient = &http.Client{Timeout: 30 * time.Second}

// configure 经 REST 创建连接/设备/点位与可选输出/告警规则。
func (e *runEnv) configure() error {
	// 1) 连接:每条连接指向各自独立的模拟从站端口。
	for i := 0; i < *flagConns; i++ {
		id := fmt.Sprintf("conn-%03d", i)
		code, err := e.post("/connections", map[string]any{
			"id": id, "name": id, "driver": "modbus",
			"config": map[string]any{"mode": "tcp", "address": e.sims[i].Addr(), "timeout": "2s"},
		})
		if err != nil || code != http.StatusCreated {
			return fmt.Errorf("create connection %s: code=%d err=%v", id, code, err)
		}
	}

	// 2) 设备:均分到各连接,每设备 points 个保持寄存器点位。
	points := make([]model.Point, *flagPoints)
	for j := 0; j < *flagPoints; j++ {
		points[j] = model.Point{
			Name:     fmt.Sprintf("p%d", j),
			Address:  fmt.Sprintf("holding:%d", j),
			DataType: model.DataTypeFloat,
			Scale:    1,
		}
	}
	// 并发创建以加速大规模配置(信号量限流 + WaitGroup 等待全部完成)。
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	var created atomic.Int64
	for i := 0; i < *flagDevices; i++ {
		wg.Add(1)
		sem <- struct{}{} // 限流:最多 16 个并发创建
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			id := fmt.Sprintf("d%05d", i)
			connID := fmt.Sprintf("conn-%03d", i%*flagConns)
			code, err := e.post("/devices", map[string]any{
				"id": id, "name": id, "connectionId": connID,
				"params":     map[string]any{"slaveId": (i % 240) + 1},
				"intervalMs": *flagInterval, "enabled": true,
				"points": points,
			})
			if err != nil || code != http.StatusCreated {
				select {
				case errCh <- fmt.Errorf("create device %s: code=%d err=%v", id, code, err):
				default:
				}
				return
			}
			n := created.Add(1)
			if n%500 == 0 {
				log.Printf("created %d devices", n)
			}
		}(i)
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
	}

	var outID string
	if *flagMQTT {
		// 3) MQTT 输出 → 假 broker。
		outID = "out-mqtt"
		code, err := e.post("/outputs", map[string]any{
			"id": outID, "name": "bench-mqtt", "type": "mqtt", "enabled": true,
			"config": map[string]any{"broker": "tcp://" + e.sink.Addr, "clientId": "scalebench", "qos": 1},
		})
		if err != nil || code != http.StatusCreated {
			return fmt.Errorf("create output: code=%d err=%v", code, err)
		}
		log.Printf("mqtt output enabled -> %s", e.sink.Addr)
	}

	// 4) 可选告警规则:引用各设备 p0,阈值取远超量程的 200000,仅评估不触发。
	if *flagAlerts > 0 {
		for i := 0; i < *flagAlerts; i++ {
			dev := fmt.Sprintf("d%05d", i%*flagDevices)
			expr := fmt.Sprintf(`point("%s","p0") > 200000`, dev)
			refs := []map[string]string{{"deviceId": dev, "point": "p0"}}
			outputs := []string{}
			if outID != "" {
				outputs = append(outputs, outID)
			}
			code, err := e.post("/alert-rules", map[string]any{
				"name": fmt.Sprintf("bench-alert-%d", i), "enabled": true, "severity": "warning",
				"expr": expr, "referencedPoints": refs, "outputIds": outputs,
				"freshnessSeconds": 300, "cooldownSeconds": 0,
			})
			if err != nil || code != http.StatusCreated {
				return fmt.Errorf("create alert rule %d: code=%d err=%v", i, code, err)
			}
		}
	}
	log.Printf("configured %d connections / %d devices / %d points each", *flagConns, *flagDevices, *flagPoints)
	return nil
}

// warmup 等待调度器把设备纳入轮询并完成首轮采集。
func (e *runEnv) warmup() error {
	deadline := time.Now().Add(*flagWarmup)
	for time.Now().Before(deadline) {
		status, err := e.statusOnline()
		if err == nil && status == *flagDevices {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	onl, _ := e.statusOnline()
	log.Printf("warmup done (online=%d/%d), starting sampling", onl, *flagDevices)
	return nil
}

// statusOnline 返回当前在线设备数(经 /api/v1/status)。
func (e *runEnv) statusOnline() (int, error) {
	resp, err := httpClient.Get(e.apiBase + "/status")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var st []struct {
		DeviceID string `json:"deviceId"`
		Online   bool   `json:"online"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return 0, err
	}
	n := 0
	for _, s := range st {
		if s.Online {
			n++
		}
	}
	return n, nil
}

// fetchMetrics 抓取并解析 /metrics。
func (e *runEnv) fetchMetrics() (*metrics, error) {
	resp, err := httpClient.Get(e.gwAddr + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseMetrics(string(body)), nil
}

// collect 在采样窗口内周期抓取快照。
func (e *runEnv) collect() []sample {
	var samples []sample
	deadline := time.Now().Add(*flagDuration)
	for time.Now().Before(deadline) {
		s, ok := e.sampleOnce()
		if ok {
			samples = append(samples, s)
		}
		time.Sleep(*flagStep)
	}
	// 补一个结束点,保证速率窗口完整
	if s, ok := e.sampleOnce(); ok {
		samples = append(samples, s)
	}
	return samples
}

// sampleOnce 采集一次快照。
func (e *runEnv) sampleOnce() (sample, bool) {
	m, err := e.fetchMetrics()
	if err != nil {
		return sample{}, false
	}
	onl, _ := e.statusOnline()
	return sample{
		t:        time.Now(),
		collect:  m.get("iot_gateway_collect_total"),
		errs:     m.get("iot_gateway_collect_errors_total"),
		online:   onl,
		rssMB:    m.get("iot_gateway_process_rss_bytes") / (1024 * 1024),
		goros:    m.get("iot_gateway_go_goroutines"),
		memUsed:  m.get("iot_gateway_mem_used_percent"),
		queueLen: m.get("iot_gateway_task_queue_length"),
		queueCap: m.get("iot_gateway_task_queue_capacity"),
		sinkMsgs: int64(len(e.sink.Messages())),
		simReqs:  e.simTotal(func(s *modbusSim) int64 { return s.reqs.Load() }),
		simReads: e.simTotal(func(s *modbusSim) int64 { return s.reads.Load() }),
		outSent:  m.get("iot_gateway_output_sent_total"),
		outDrop:  m.get("iot_gateway_output_dropped_total"),
		outPend:  m.get("iot_gateway_output_pending"),
		procIn:   m.get("iot_gateway_processing_points_in_total"),
		procPass: m.get("iot_gateway_processing_points_passed_total"),
		procFilt: m.get("iot_gateway_processing_points_filtered_total"),
		procAggr: m.get("iot_gateway_processing_points_aggregated_total"),
	}, true
}

// report 汇总并打印结果。
func report(samples []sample) {
	if len(samples) < 2 {
		log.Fatal("采样点不足,无法计算速率")
	}
	first, last := samples[0], samples[len(samples)-1]
	dt := last.t.Sub(first.t).Seconds()
	rate := func(a, b float64) float64 {
		if dt <= 0 {
			return 0
		}
		return (b - a) / dt
	}

	// 每步速率(min/max,观察抖动)
	var collectStepMin, collectStepMax float64
	for i := 1; i < len(samples); i++ {
		r := (samples[i].collect - samples[i-1].collect) / samples[i].t.Sub(samples[i-1].t).Seconds()
		if i == 1 || r < collectStepMin {
			collectStepMin = r
		}
		if r > collectStepMax {
			collectStepMax = r
		}
	}

	var sb strings.Builder
	w := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Println(line)
		sb.WriteString(line + "\n")
	}

	w("===== scalebench 压测汇总 =====")
	w("规模:devices=%d × points=%d,conns=%d,pool=%d,interval=%dms",
		*flagDevices, *flagPoints, *flagConns, *flagPool, *flagInterval)
	w("窗口:%s(预热 %s),采样 %d 点", last.t.Sub(first.t).Round(100*time.Millisecond), *flagWarmup, len(samples))
	w("")
	w("── 采集(南向)──")
	w("  collect_total      = %.0f(+%.0f)", last.collect, last.collect-first.collect)
	w("  collect 速率       = %.1f 次/秒(单步区间 %.1f ~ %.1f)", rate(first.collect, last.collect), collectStepMin, collectStepMax)
	w("  采集错误           = %.0f(+%.0f)", last.errs, last.errs-first.errs)
	w("  在线设备           = %d/%d", last.online, *flagDevices)
	w("  预估数据点吞吐     ≈ %.0f 点/秒(collect速率×每设备点数)", rate(first.collect, last.collect)*float64(*flagPoints))
	if last.simReads > 0 {
		w("  Modbus 读请求     = %.1f 请求/秒(模拟从站实测,寄存器 %.0f 个)",
			rate(float64(first.simReqs), float64(last.simReqs)), rate(float64(first.simReads), float64(last.simReads)))
	}
	w("")
	w("── 北向输出 ──")
	if *flagMQTT {
		w("  假 broker 收到     = %d 条(+%d,%.1f 条/秒)",
			last.sinkMsgs, last.sinkMsgs-first.sinkMsgs, float64(last.sinkMsgs-first.sinkMsgs)/dt)
		w("  输出 sent_total    = %.0f(+%.0f,%.1f 条/秒)", last.outSent, last.outSent-first.outSent, rate(first.outSent, last.outSent))
		w("  输出 dropped/pending = %.0f / %.0f", last.outDrop, last.outPend)
	} else {
		w("  未启用 MQTT 输出(未测北向)")
	}
	w("")
	w("── 运行时(资源)──")
	w("  RSS               = %.1f MB", last.rssMB)
	w("  goroutines        = %.0f", last.goros)
	w("  系统内存占用      = %.1f %%", last.memUsed)
	w("  任务队列 len/cap  = %.0f / %.0f", last.queueLen, last.queueCap)
	if last.procIn > 0 {
		w("  处理层 in/pass/filtered/aggregated = %.0f / %.0f / %.0f / %.0f",
			last.procIn, last.procPass, last.procFilt, last.procAggr)
	}
	w("============================")

	if *flagOut != "" {
		lines := []string{
			"devices,points,conns,pool,interval_ms,collect_rate,collect_errors,online,rss_mb,goroutines,sink_msgs,sim_reads",
			fmt.Sprintf("%d,%d,%d,%d,%d,%.2f,%.0f,%d,%.1f,%.0f,%d,%d",
				*flagDevices, *flagPoints, *flagConns, *flagPool, *flagInterval,
				rate(first.collect, last.collect), last.errs, last.online,
				last.rssMB, last.goros, last.sinkMsgs, last.simReads),
		}
		if err := os.WriteFile(*flagOut, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			log.Printf("写 CSV 报告失败: %v", err)
		} else {
			log.Printf("CSV 报告已写入 %s", *flagOut)
		}
	}
}

// cleanup 关闭网关子进程与模拟器,清理临时目录。
func (e *runEnv) cleanup() {
	if e.gwCancel != nil {
		e.gwCancel()
		if e.gwCmd != nil {
			done := make(chan struct{})
			go func() { e.gwCmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				e.gwCmd.Process.Kill()
			}
		}
	}
	if e.sink != nil {
		e.sink.Close()
	}
	for _, sim := range e.sims {
		sim.Close()
	}
	if e.tmpDir != "" {
		os.RemoveAll(e.tmpDir)
	}
}

// simTotal 汇总所有模拟从站的某计数器。
func (e *runEnv) simTotal(f func(*modbusSim) int64) int64 {
	var total int64
	for _, s := range e.sims {
		total += f(s)
	}
	return total
}
