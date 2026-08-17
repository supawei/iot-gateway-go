// Package tdengine 实现 TDengine 时序数据库对接(北向输出插件)。
// 通过 taosAdapter 的 REST API(/rest/sql)写入,无需 CGO 驱动,纯 Go 依赖。
//
// 数据模型:一个超级表(STable)存储所有点位,子表按 (deviceID, point) 自动建表,
// 设备与点位名作为 TAGS,便于按设备/点位过滤与分区。值按 Go 类型写入对应强类型列
// (DOUBLE / BIGINT / BOOL / NCHAR),quality 单独一列记录数据质量。
// 详见 docs/tdengine.md。
package tdengine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

const (
	defaultFlushInterval = time.Second
	defaultDatabase      = "iot_gateway"
	defaultStable        = "data_points"

	// REST 执行 SQL 的路径;taosAdapter 默认监听 6041。
	restSQLPath = "/rest/sql"

	// httpTimeout 是单次 REST 请求超时;采集数据写入属可重试操作,失败由 flush 记录并丢弃。
	httpTimeout = 10 * time.Second
)

// Config 是 TDengine 输出的配置(存 SQLite,经 Web UI 配置)。
// 同时保留 yaml tag 以兼容旧 config.yaml 的一次性迁移(见 main.migrateOutputs)。
type Config struct {
	URL           string `json:"url" yaml:"url"`                     // taosAdapter REST 地址,如 http://127.0.0.1:6041
	Username      string `json:"username" yaml:"username"`           // 默认 root
	Password      string `json:"password" yaml:"password"`           // 默认 taosdata
	Database      string `json:"database" yaml:"database"`           // 库名,默认 iot_gateway
	Stable        string `json:"stable" yaml:"stable"`               // 超级表名,默认 data_points
	FlushInterval string `json:"flushInterval" yaml:"flushInterval"` // 微批聚合 flush 间隔,默认 1s
}

// init 注册 TDengine 输出类型:声明配置 schema 并绑定构造器。
func init() {
	output.Register(output.Descriptor{
		Type:  "tdengine",
		Label: "TDengine",
		Schema: []output.Field{
			{Name: "url", Label: "REST 地址", Type: output.FieldString, Required: true, Placeholder: "http://127.0.0.1:6041", Hint: "taosAdapter REST 地址"},
			{Name: "username", Label: "用户名", Type: output.FieldString, Default: "root"},
			{Name: "password", Label: "密码", Type: output.FieldPassword, Default: "taosdata"},
			{Name: "database", Label: "数据库", Type: output.FieldString, Default: "iot_gateway"},
			{Name: "stable", Label: "超级表", Type: output.FieldString, Default: "data_points"},
			{Name: "flushInterval", Label: "Flush 间隔", Type: output.FieldString, Default: "1s", Hint: "微批聚合写入间隔,如 1s"},
		},
	}, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("tdengine config: %w", err)
		}
		return New(cfg)
	})
}

type tdengineOutput struct {
	client        *http.Client
	baseURL       string
	user, pass    string
	db, stable    string
	flushInterval time.Duration

	// pending 缓冲待写入点位;flush 单 goroutine 串行消费,created 亦仅由其访问。
	mu      sync.Mutex
	pending []model.DataPoint

	created map[string]bool // 已建子表名(flusher goroutine 专用)

	done chan struct{}
	wg   sync.WaitGroup
}

// New 构造 TDengine 输出:校验配置、建库建表(幂等)、启动 flusher goroutine。
// 建库建表失败即返回错误(与 mqtt/thingsboard 在 New 时校验连接一致)。
func New(cfg Config) (output.Output, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("tdengine url is required")
	}
	if cfg.Username == "" {
		cfg.Username = "root"
	}
	if cfg.Password == "" {
		cfg.Password = "taosdata"
	}
	if cfg.Database == "" {
		cfg.Database = defaultDatabase
	}
	if cfg.Stable == "" {
		cfg.Stable = defaultStable
	}
	flushInterval := defaultFlushInterval
	if cfg.FlushInterval != "" {
		d, err := time.ParseDuration(cfg.FlushInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid tdengine flushInterval %q: %w", cfg.FlushInterval, err)
		}
		flushInterval = d
	}

	o := &tdengineOutput{
		client:        &http.Client{Timeout: httpTimeout},
		baseURL:       strings.TrimRight(cfg.URL, "/"),
		user:          cfg.Username,
		pass:          cfg.Password,
		db:            cfg.Database,
		stable:        cfg.Stable,
		flushInterval: flushInterval,
		created:       make(map[string]bool),
		done:          make(chan struct{}),
	}

	// 幂等建库建表,兼作启动连通性校验。
	if err := o.exec(createDatabaseSQL(o.db)); err != nil {
		return nil, fmt.Errorf("tdengine create database: %w", err)
	}
	if err := o.exec(createStableSQL(o.db, o.stable)); err != nil {
		return nil, fmt.Errorf("tdengine create stable: %w", err)
	}

	o.wg.Add(1)
	go o.runFlusher()
	return o, nil
}

// Publish 把 DataPoint 缓冲进待写队列,由 flusher 定时聚合为批量 INSERT。
// 含 nil 值的点(bad/uncertain)也记录,以 quality 列标记数据质量。
func (o *tdengineOutput) Publish(dp model.DataPoint) error {
	o.mu.Lock()
	o.pending = append(o.pending, dp)
	o.mu.Unlock()
	return nil
}

// Close 停止 flusher,写尽剩余缓冲后关闭空闲连接。
func (o *tdengineOutput) Close() error {
	close(o.done)
	o.wg.Wait()
	o.flush()
	o.client.CloseIdleConnections()
	return nil
}

// runFlusher 按 flushInterval 周期性 flush,直到 Close 关闭 done。
func (o *tdengineOutput) runFlusher() {
	defer o.wg.Done()
	ticker := time.NewTicker(o.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.done:
			return
		case <-ticker.C:
			o.flush()
		}
	}
}

// flush 取走当前缓冲,按子表分组,每组先确保子表存在(幂等),再执行一条多行 INSERT。
// 失败仅记录日志并丢弃该组(不重试、不阻塞采集侧),保持与其他输出的背压隔离语义一致。
func (o *tdengineOutput) flush() {
	o.mu.Lock()
	pending := o.pending
	o.pending = nil
	o.mu.Unlock()
	if len(pending) == 0 {
		return
	}

	groups := groupByChild(pending)
	keys := make([]childKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].deviceID != keys[j].deviceID {
			return keys[i].deviceID < keys[j].deviceID
		}
		return keys[i].point < keys[j].point
	})

	for _, k := range keys {
		child := childTable(k.deviceID, k.point)
		if !o.created[child] {
			if err := o.exec(createChildSQL(o.db, o.stable, child, k.deviceID, k.point)); err != nil {
				slog.Error("tdengine create child table failed", "device", k.deviceID, "point", k.point, "err", err)
				continue
			}
			o.created[child] = true
		}
		if err := o.exec(buildInsertSQL(o.db, child, groups[k])); err != nil {
			slog.Error("tdengine insert failed", "device", k.deviceID, "point", k.point, "err", err)
		}
	}
}

// exec 通过 taosAdapter REST 执行一条 SQL;HTTP 非 2xx 或返回 code!=0 视为失败。
func (o *tdengineOutput) exec(sql string) error {
	req, err := http.NewRequest(http.MethodPost, o.baseURL+restSQLPath, bytes.NewBufferString(sql))
	if err != nil {
		return fmt.Errorf("tdengine build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.SetBasicAuth(o.user, o.pass)

	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("tdengine request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var r struct {
		Code int    `json:"code"`
		Desc string `json:"desc"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("tdengine decode response (http %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("tdengine http %d: %s", resp.StatusCode, r.Desc)
	}
	if r.Code != 0 {
		return fmt.Errorf("tdengine code %d: %s", r.Code, r.Desc)
	}
	return nil
}

// childKey 是子表分组键:一台设备的一个点位对应一张子表。
type childKey struct {
	deviceID string
	point    string
}

func groupByChild(points []model.DataPoint) map[childKey][]model.DataPoint {
	groups := make(map[childKey][]model.DataPoint)
	for _, dp := range points {
		k := childKey{deviceID: dp.DeviceID, point: dp.Point}
		groups[k] = append(groups[k], dp)
	}
	return groups
}

// childTable 由 (deviceID, point) 派生稳定、合法的子表名。
// TAGS 已携带可读的 deviceID/point,故子表名可用 hash 保证唯一性与合法性(避免设备/点位名中的非法字符)。
func childTable(deviceID, point string) string {
	h := fnv.New64a()
	h.Write([]byte(deviceID))
	h.Write([]byte{0})
	h.Write([]byte(point))
	return "t_" + fmt.Sprintf("%016x", h.Sum64())
}

func ident(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// escapeString 按 TDengine 规则转义字符串字面量:反斜杠转义。
func escapeString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

// valueLiteral 把采集值按 Go 类型映射到强类型列,返回列名与 SQL 字面量;
// nil/不支持的类型返回 ok=false(该行所有值列写 NULL)。
func valueLiteral(v interface{}) (col, lit string, ok bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return "v_bool", "true", true
		}
		return "v_bool", "false", true
	case int:
		return "v_int", strconv.FormatInt(int64(t), 10), true
	case int8:
		return "v_int", strconv.FormatInt(int64(t), 10), true
	case int16:
		return "v_int", strconv.FormatInt(int64(t), 10), true
	case int32:
		return "v_int", strconv.FormatInt(int64(t), 10), true
	case int64:
		return "v_int", strconv.FormatInt(t, 10), true
	case uint:
		return "v_int", strconv.FormatUint(uint64(t), 10), true
	case uint8:
		return "v_int", strconv.FormatUint(uint64(t), 10), true
	case uint16:
		return "v_int", strconv.FormatUint(uint64(t), 10), true
	case uint32:
		return "v_int", strconv.FormatUint(uint64(t), 10), true
	case uint64:
		return "v_int", strconv.FormatUint(t, 10), true
	case float32:
		return "v_double", strconv.FormatFloat(float64(t), 'f', -1, 32), true
	case float64:
		return "v_double", strconv.FormatFloat(t, 'f', -1, 64), true
	case string:
		return "v_str", "'" + escapeString(t) + "'", true
	default:
		return "", "", false
	}
}

// columns 是 INSERT 的列顺序(与建表语句一致);值列按类型四选一,其余 NULL。
var columns = []string{"ts", "quality", "v_double", "v_int", "v_bool", "v_str"}

// rowTuple 把单个 DataPoint 构造成一行 VALUES 元组。
func rowTuple(dp model.DataPoint) string {
	ts := dp.Timestamp.UnixMilli()
	if dp.Timestamp.IsZero() {
		ts = time.Now().UnixMilli()
	}

	// 值列默认 NULL,按类型填四列之一。
	vals := []string{
		strconv.FormatInt(ts, 10),
		"'" + escapeString(string(dp.Quality)) + "'",
		"NULL", "NULL", "NULL", "NULL",
	}
	if col, lit, ok := valueLiteral(dp.Value); ok {
		switch col {
		case "v_double":
			vals[2] = lit
		case "v_int":
			vals[3] = lit
		case "v_bool":
			vals[4] = lit
		case "v_str":
			vals[5] = lit
		}
	}
	return "(" + strings.Join(vals, ",") + ")"
}

func createDatabaseSQL(db string) string {
	return fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", ident(db))
}

// createStableSQL 建超级表:值列按类型四选一 + quality + ts,TAGS 携带设备与点位。
func createStableSQL(db, stable string) string {
	return fmt.Sprintf(
		"CREATE STABLE IF NOT EXISTS %s.%s ("+
			"%s TIMESTAMP, %s NCHAR(16), %s DOUBLE, %s BIGINT, %s BOOL, %s NCHAR(4096)"+
			") TAGS (%s NCHAR(128), %s NCHAR(128))",
		ident(db), ident(stable),
		ident("ts"), ident("quality"), ident("v_double"), ident("v_int"), ident("v_bool"), ident("v_str"),
		ident("device_id"), ident("point"),
	)
}

// createChildSQL 幂等创建子表并打 TAGS。
func createChildSQL(db, stable, child, deviceID, point string) string {
	return fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s.%s USING %s.%s TAGS ('%s','%s')",
		ident(db), ident(child), ident(db), ident(stable),
		escapeString(deviceID), escapeString(point),
	)
}

// buildInsertSQL 构造一条多行 INSERT:同一子表的多条数据合并成一次写入。
func buildInsertSQL(db, child string, points []model.DataPoint) string {
	var sb strings.Builder
	sb.WriteString("INSERT INTO ")
	sb.WriteString(ident(db) + "." + ident(child))
	sb.WriteString(" (")
	for i, c := range columns {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(ident(c))
	}
	sb.WriteString(") VALUES ")
	for i, dp := range points {
		if i > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(rowTuple(dp))
	}
	return sb.String()
}
