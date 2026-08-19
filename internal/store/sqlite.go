package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"iot-gateway-go/internal/backfill"
	"iot-gateway-go/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS connection (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    driver     TEXT NOT NULL,
    config     TEXT NOT NULL,
    managed_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS device (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    params        TEXT NOT NULL DEFAULT '{}',
    interval_ms   INTEGER NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    managed_by    TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (connection_id) REFERENCES connection(id) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS point (
    device_id   TEXT NOT NULL,
    name        TEXT NOT NULL,
    address     TEXT NOT NULL,
    data_type   TEXT NOT NULL,
    scale       REAL NOT NULL DEFAULT 0,
    processing  TEXT,
    seq         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (device_id, name),
    FOREIGN KEY (device_id) REFERENCES device(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS user (
    id                   TEXT PRIMARY KEY,
    password_hash        TEXT NOT NULL,
    must_change_password INTEGER NOT NULL DEFAULT 1,
    enabled              INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS client (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    api_key_hash TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '[]',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS output (
    id      TEXT PRIMARY KEY,
    name    TEXT NOT NULL,
    type    TEXT NOT NULL,
    config  TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS alert_rule (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    enabled            INTEGER NOT NULL DEFAULT 1,
    severity           TEXT NOT NULL DEFAULT 'warning',
    expr               TEXT NOT NULL,
    referenced_points  TEXT NOT NULL DEFAULT '[]',
    output_ids         TEXT NOT NULL DEFAULT '[]',
    freshness_seconds  INTEGER NOT NULL DEFAULT 300,
    cooldown_seconds   INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS alert (
    alert_id     TEXT PRIMARY KEY,
    rule_id      TEXT NOT NULL,
    rule_name    TEXT NOT NULL,
    severity     TEXT NOT NULL,
    message      TEXT NOT NULL,
    triggered_at TEXT NOT NULL,
    gateway_id   TEXT NOT NULL,
    context      TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    resolved_at  TEXT
);`

// ErrConnectionInUse 表示连接仍被设备引用,不可删除。
var ErrConnectionInUse = errors.New("connection is referenced by devices")

// 网关级设置项 key(settings 表)。
const (
	// SettingGatewayID 是网关 ID,默认 DefaultGatewayID,管理员可经 Web UI 修改。
	SettingGatewayID = "gateway.id"
)

// DefaultGatewayID 是首次启动预置的默认网关 ID(与旧 config.yaml 默认值一致)。
const DefaultGatewayID = "iot-gateway"

// isDuplicateColumn 判断 SQLite 报错是否为"重复添加已存在列"(幂等 ALTER 忽略)。
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// Store 负责连接/设备/点位配置的持久化与变更通知。
type Store struct {
	db *sql.DB

	mu   sync.Mutex
	subs map[int]chan struct{} // 配置变更信号订阅者(多消费者:调度器/处理引擎)
	next int
}

func Open(path string) (*Store, error) {
	dsn := dsnWithPragmas(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// 开发期结构演进:为历史 point 表补 processing / seq 列(幂等;新库已含该列时报错忽略)。
	// 见 docs/development-conventions.md(未发布不做版本化迁移)。
	if _, err := db.Exec(`ALTER TABLE point ADD COLUMN processing TEXT`); err != nil && !isDuplicateColumn(err) {
		db.Close()
		return nil, fmt.Errorf("evolve point schema: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE point ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`); err != nil && !isDuplicateColumn(err) {
		db.Close()
		return nil, fmt.Errorf("evolve point schema: %w", err)
	}
	// 开发期结构演进:为历史 connection/device 表补 managed_by 列(幂等;标记
	// 平台同步创建的实体,取代 settings 表的 JSON 登记,见 internal/output/smardaten/sync.go)。
	if _, err := db.Exec(`ALTER TABLE connection ADD COLUMN managed_by TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumn(err) {
		db.Close()
		return nil, fmt.Errorf("evolve connection schema: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE device ADD COLUMN managed_by TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumn(err) {
		db.Close()
		return nil, fmt.Errorf("evolve device schema: %w", err)
	}
	// 预置默认网关设置(幂等):数据库为空时内置默认网关 ID。
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`,
		SettingGatewayID, DefaultGatewayID,
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrap gateway settings: %w", err)
	}
	return &Store{db: db, subs: make(map[int]chan struct{})}, nil
}

// dsnWithPragmas 为连接串附加并发相关的 PRAGMA：
//   - busy_timeout: 写锁竞争时等待而不是立即报 SQLITE_BUSY（多协程并发写必需）
//   - journal_mode(WAL): 读写并发不互相阻塞（WAL 允许读与写同时进行）
//
// :memory: 数据库无需（也不适用）这些 pragma，原样返回。
func dsnWithPragmas(path string) string {
	if path == ":memory:" || strings.Contains(path, "?") {
		return path
	}
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

func (s *Store) Close() error {
	return s.db.Close()
}

// NewBackfillStore 构造断网补传持久化队列,复用网关同一 SQLite 连接
// (WAL/busy_timeout 已生效)。max<=0 时用 backfill.DefaultMax。
// 见 docs/offline-backfill-design.md。
func (s *Store) NewBackfillStore(max int) (*backfill.Store, error) {
	return backfill.New(s.db, max)
}

// OnChange 订阅配置变更信号:每次调用注册一个新的 buffered channel,
// 写入后非阻塞通知(缓冲满即丢,语义同前);调用方退出时应调用返回的 cancel 退订。
// 支持多消费者(调度器、处理引擎等各自订阅),见 docs/edge-computing-design.md §3.1。
func (s *Store) OnChange() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	id := s.next
	s.next++
	s.subs[id] = ch
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
	return ch, cancel
}

func (s *Store) notify() {
	s.mu.Lock()
	subs := make([]chan struct{}, 0, len(s.subs))
	for _, ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // 已有待处理信号则丢弃,避免堆积
		}
	}
}

func (s *Store) SaveConnection(conn model.Connection) error {
	_, err := s.db.Exec(
		`INSERT INTO connection (id, name, driver, config, managed_by) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, driver=excluded.driver, config=excluded.config, managed_by=excluded.managed_by`,
		conn.ID, conn.Name, conn.Driver, string(conn.Config), conn.ManagedBy,
	)
	if err != nil {
		return fmt.Errorf("upsert connection: %w", err)
	}
	s.notify()
	return nil
}

func (s *Store) ListConnections() ([]model.Connection, error) {
	rows, err := s.db.Query("SELECT id, name, driver, config, managed_by FROM connection")
	if err != nil {
		return nil, fmt.Errorf("query connections: %w", err)
	}
	defer rows.Close()
	conns := make([]model.Connection, 0)
	for rows.Next() {
		conn, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		conns = append(conns, conn)
	}
	return conns, rows.Err()
}

func (s *Store) GetConnection(id string) (model.Connection, error) {
	row := s.db.QueryRow("SELECT id, name, driver, config, managed_by FROM connection WHERE id = ?", id)
	conn, err := scanConnection(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Connection{}, fmt.Errorf("connection %q not found", id)
		}
		return model.Connection{}, fmt.Errorf("get connection: %w", err)
	}
	return conn, nil
}

// ListManagedConnectionIDs 列出由 manager 自动创建管理的连接 ID(用于平台同步
// 的孤儿清理;手工配置的连接 managed_by 为空,不会出现在结果中)。
func (s *Store) ListManagedConnectionIDs(manager string) ([]string, error) {
	rows, err := s.db.Query("SELECT id FROM connection WHERE managed_by = ?", manager)
	if err != nil {
		return nil, fmt.Errorf("query managed connections: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan managed connection: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteConnection 删除连接;若有 device 引用则返回 ErrConnectionInUse。
func (s *Store) DeleteConnection(id string) error {
	var refCount int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM device WHERE connection_id = ?", id).Scan(&refCount); err != nil {
		return fmt.Errorf("check connection references: %w", err)
	}
	if refCount > 0 {
		return fmt.Errorf("%w: %d device(s)", ErrConnectionInUse, refCount)
	}
	if _, err := s.db.Exec("DELETE FROM connection WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	s.notify()
	return nil
}

func (s *Store) ListDevices() ([]model.Device, error) {
	devices, err := s.queryDevices("SELECT id, name, connection_id, params, interval_ms, enabled, managed_by FROM device")
	if err != nil {
		return nil, err
	}
	pointsByDevice, err := s.queryAllPoints()
	if err != nil {
		return nil, err
	}
	for index := range devices {
		points := pointsByDevice[devices[index].ID]
		if points == nil {
			points = []model.Point{}
		}
		devices[index].Points = points
	}
	return devices, nil
}

func (s *Store) GetDevice(id string) (model.Device, error) {
	devices, err := s.queryDevices("SELECT id, name, connection_id, params, interval_ms, enabled, managed_by FROM device WHERE id = ?", id)
	if err != nil {
		return model.Device{}, err
	}
	if len(devices) == 0 {
		return model.Device{}, fmt.Errorf("device %q not found", id)
	}
	device := devices[0]
	device.Points, err = s.queryPoints("SELECT name, address, data_type, scale, processing FROM point WHERE device_id = ? ORDER BY seq", id)
	if err != nil {
		return model.Device{}, err
	}
	return device, nil
}

func (s *Store) SaveDevice(device model.Device) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := saveDeviceTx(tx, device); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.notify()
	return nil
}

func (s *Store) DeleteDevice(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM point WHERE device_id = ?", id); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete points: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM device WHERE id = ?", id); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete device: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.notify()
	return nil
}

// ListManagedDeviceIDs 列出由 manager 自动创建管理的设备 ID(用于平台同步的
// 孤儿清理;手工配置的设备 managed_by 为空,不会出现在结果中)。
func (s *Store) ListManagedDeviceIDs(manager string) ([]string, error) {
	rows, err := s.db.Query("SELECT id FROM device WHERE managed_by = ?", manager)
	if err != nil {
		return nil, fmt.Errorf("query managed devices: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan managed device: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetDeviceEnabled 批量启停用:只改 enabled 字段,不动 params/points。
// 设备不存在时返回 not-found 错误(与 GetDevice 语义一致,供 batch 逐条报错)。
func (s *Store) SetDeviceEnabled(id string, enabled bool) error {
	enabledVal := 0
	if enabled {
		enabledVal = 1
	}
	res, err := s.db.Exec("UPDATE device SET enabled = ? WHERE id = ?", enabledVal, id)
	if err != nil {
		return fmt.Errorf("update device enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("device %q not found", id)
	}
	s.notify()
	return nil
}

func (s *Store) AddPoint(deviceID string, point model.Point) error {
	// 追加到末尾:seq 取该设备当前最大序号 +1,保持用户定义的点位顺序。
	var maxSeq int
	if err := s.db.QueryRow("SELECT COALESCE(MAX(seq), 0) FROM point WHERE device_id = ?", deviceID).Scan(&maxSeq); err != nil {
		return fmt.Errorf("query point seq: %w", err)
	}
	_, err := s.db.Exec(
		"INSERT INTO point (device_id, name, address, data_type, scale, processing, seq) VALUES (?, ?, ?, ?, ?, ?, ?)",
		deviceID, point.Name, point.Address, string(point.DataType), point.Scale, marshalProcessing(point.Processing), maxSeq+1,
	)
	if err != nil {
		return fmt.Errorf("insert point: %w", err)
	}
	s.notify()
	return nil
}

func (s *Store) DeletePoint(deviceID, name string) error {
	if _, err := s.db.Exec("DELETE FROM point WHERE device_id = ? AND name = ?", deviceID, name); err != nil {
		return fmt.Errorf("delete point: %w", err)
	}
	s.notify()
	return nil
}

// ---- 用户 ----

// SaveUser 插入或更新用户(upsert)。密码哈希与改密标志由上层(auth)计算。
func (s *Store) SaveUser(u model.User) error {
	mustChange, enabled := 0, 0
	if u.MustChangePassword {
		mustChange = 1
	}
	if u.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO user (id, password_hash, must_change_password, enabled) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET password_hash=excluded.password_hash, must_change_password=excluded.must_change_password, enabled=excluded.enabled`,
		u.ID, u.PasswordHash, mustChange, enabled,
	)
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	return nil
}

// GetUser 返回用户;不存在返回 sql.ErrNoRows 包装的错误。
func (s *Store) GetUser(id string) (model.User, error) {
	row := s.db.QueryRow("SELECT id, password_hash, must_change_password, enabled FROM user WHERE id = ?", id)
	var u model.User
	var mustChange, enabled int
	if err := row.Scan(&u.ID, &u.PasswordHash, &mustChange, &enabled); err != nil {
		return model.User{}, fmt.Errorf("get user %q: %w", id, err)
	}
	u.MustChangePassword = mustChange != 0
	u.Enabled = enabled != 0
	return u, nil
}

// CountUsers 返回用户数,用于判断是否需要预置管理员。
func (s *Store) CountUsers() (int, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM user").Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// ---- 三方 client ----

// SaveClient 插入或更新三方 client;scope 以 JSON 数组存。
func (s *Store) SaveClient(c model.Client) error {
	scopes, err := json.Marshal(c.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	_, err = s.db.Exec(
		`INSERT INTO client (id, name, api_key_hash, scopes, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, api_key_hash=excluded.api_key_hash, scopes=excluded.scopes, enabled=excluded.enabled`,
		c.ID, c.Name, c.APIKeyHash, string(scopes), enabled, c.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert client: %w", err)
	}
	return nil
}

// GetClient 返回三方 client;不存在返回 sql.ErrNoRows 包装的错误。
func (s *Store) GetClient(id string) (model.Client, error) {
	row := s.db.QueryRow("SELECT id, name, api_key_hash, scopes, enabled, created_at FROM client WHERE id = ?", id)
	c, err := scanClient(row)
	if err != nil {
		return model.Client{}, fmt.Errorf("get client %q: %w", id, err)
	}
	return c, nil
}

// GetClientByKeyHash 按 API Key 的 SHA-256 哈希查找三方 client(认证用)。
func (s *Store) GetClientByKeyHash(hash string) (model.Client, bool) {
	row := s.db.QueryRow("SELECT id, name, api_key_hash, scopes, enabled, created_at FROM client WHERE api_key_hash = ?", hash)
	c, err := scanClient(row)
	if err != nil {
		return model.Client{}, false
	}
	return c, true
}

// ListClients 返回全部三方 client,按 ID 排序。
func (s *Store) ListClients() ([]model.Client, error) {
	rows, err := s.db.Query("SELECT id, name, api_key_hash, scopes, enabled, created_at FROM client ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query clients: %w", err)
	}
	defer rows.Close()
	clients := make([]model.Client, 0)
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}

// DeleteClient 删除三方 client。
func (s *Store) DeleteClient(id string) error {
	if _, err := s.db.Exec("DELETE FROM client WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete client: %w", err)
	}
	return nil
}

// ---- 北向输出 ----

// SaveOutput 插入或更新输出配置(upsert);写入后通知热重载。
func (s *Store) SaveOutput(o model.Output) error {
	enabled := 0
	if o.Enabled {
		enabled = 1
	}
	config := o.Config
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	_, err := s.db.Exec(
		`INSERT INTO output (id, name, type, config, enabled) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type, config=excluded.config, enabled=excluded.enabled`,
		o.ID, o.Name, o.Type, string(config), enabled,
	)
	if err != nil {
		return fmt.Errorf("upsert output: %w", err)
	}
	s.notify()
	return nil
}

// ListOutputs 返回全部输出配置,按 ID 排序。
func (s *Store) ListOutputs() ([]model.Output, error) {
	rows, err := s.db.Query("SELECT id, name, type, config, enabled FROM output ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query outputs: %w", err)
	}
	defer rows.Close()
	outputs := make([]model.Output, 0)
	for rows.Next() {
		o, err := scanOutput(rows)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, o)
	}
	return outputs, rows.Err()
}

// GetOutput 返回单个输出配置;不存在返回 sql.ErrNoRows 包装的错误。
func (s *Store) GetOutput(id string) (model.Output, error) {
	row := s.db.QueryRow("SELECT id, name, type, config, enabled FROM output WHERE id = ?", id)
	o, err := scanOutput(row)
	if err != nil {
		return model.Output{}, fmt.Errorf("get output %q: %w", id, err)
	}
	return o, nil
}

// DeleteOutput 删除输出配置。
func (s *Store) DeleteOutput(id string) error {
	if _, err := s.db.Exec("DELETE FROM output WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete output: %w", err)
	}
	s.notify()
	return nil
}

func scanOutput(row rowScanner) (model.Output, error) {
	var o model.Output
	var config string
	var enabled int
	if err := row.Scan(&o.ID, &o.Name, &o.Type, &config, &enabled); err != nil {
		return model.Output{}, err
	}
	o.Config = json.RawMessage(config)
	o.Enabled = enabled != 0
	return o, nil
}

// ---- 网关设置(键值) ----

// GetSetting 读取网关设置;不存在返回 ("", false, nil)。
func (s *Store) GetSetting(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get setting %q: %w", key, err)
	}
	return value, true, nil
}

// SetSetting 写入网关设置(upsert)。
func (s *Store) SetSetting(key, value string) error {
	if _, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value,
	); err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

// GetGatewayID 返回网关 ID;未设置时回退默认值(正常由 Open 预置,防御性兜底)。
func (s *Store) GetGatewayID() (string, error) {
	v, ok, err := s.GetSetting(SettingGatewayID)
	if err != nil {
		return "", err
	}
	if !ok || v == "" {
		return DefaultGatewayID, nil
	}
	return v, nil
}

func scanClient(row rowScanner) (model.Client, error) {
	var c model.Client
	var scopes string
	var enabled int
	if err := row.Scan(&c.ID, &c.Name, &c.APIKeyHash, &scopes, &enabled, &c.CreatedAt); err != nil {
		return model.Client{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &c.Scopes); err != nil {
		return model.Client{}, fmt.Errorf("unmarshal scopes: %w", err)
	}
	c.Enabled = enabled != 0
	return c, nil
}

func (s *Store) queryDevices(query string, args ...any) ([]model.Device, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	defer rows.Close()
	devices := make([]model.Device, 0)
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Store) queryPoints(query string, args ...any) ([]model.Point, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query points: %w", err)
	}
	defer rows.Close()
	points := make([]model.Point, 0)
	for rows.Next() {
		point, err := scanPoint(rows)
		if err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

func (s *Store) queryAllPoints() (map[string][]model.Point, error) {
	rows, err := s.db.Query("SELECT device_id, name, address, data_type, scale, processing FROM point ORDER BY device_id, seq")
	if err != nil {
		return nil, fmt.Errorf("query all points: %w", err)
	}
	defer rows.Close()
	result := make(map[string][]model.Point)
	for rows.Next() {
		var deviceID, dataType string
		var point model.Point
		var processing sql.NullString
		if err := rows.Scan(&deviceID, &point.Name, &point.Address, &dataType, &point.Scale, &processing); err != nil {
			return nil, fmt.Errorf("scan point: %w", err)
		}
		point.DataType = model.DataType(dataType)
		point.Processing = unmarshalProcessing(processing.String)
		result[deviceID] = append(result[deviceID], point)
	}
	return result, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConnection(row rowScanner) (model.Connection, error) {
	var conn model.Connection
	var config string
	if err := row.Scan(&conn.ID, &conn.Name, &conn.Driver, &config, &conn.ManagedBy); err != nil {
		return model.Connection{}, fmt.Errorf("scan connection: %w", err)
	}
	conn.Config = json.RawMessage(config)
	return conn, nil
}

func scanDevice(row rowScanner) (model.Device, error) {
	var device model.Device
	var connectionID, params string
	var enabled int
	if err := row.Scan(&device.ID, &device.Name, &connectionID, &params, &device.IntervalMs, &enabled, &device.ManagedBy); err != nil {
		return model.Device{}, fmt.Errorf("scan device: %w", err)
	}
	device.ConnectionID = connectionID
	device.Params = json.RawMessage(params)
	device.Enabled = enabled != 0
	return device, nil
}

func scanPoint(row rowScanner) (model.Point, error) {
	var point model.Point
	var dataType string
	var processing sql.NullString
	if err := row.Scan(&point.Name, &point.Address, &dataType, &point.Scale, &processing); err != nil {
		return model.Point{}, fmt.Errorf("scan point: %w", err)
	}
	point.DataType = model.DataType(dataType)
	point.Processing = unmarshalProcessing(processing.String)
	return point, nil
}

// marshalProcessing 把点位处理配置序列化为 JSON 字符串;nil 返回空串(存 NULL)。
func marshalProcessing(p *model.PointProcessing) string {
	if p == nil {
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// unmarshalProcessing 反序列化点位处理配置;空串返回 nil。
func unmarshalProcessing(s string) *model.PointProcessing {
	if s == "" {
		return nil
	}
	var p model.PointProcessing
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil
	}
	return &p
}

func saveDeviceTx(tx *sql.Tx, device model.Device) error {
	enabled := 0
	if device.Enabled {
		enabled = 1
	}
	params := device.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(
		`INSERT INTO device (id, name, connection_id, params, interval_ms, enabled, managed_by) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, connection_id=excluded.connection_id, params=excluded.params, interval_ms=excluded.interval_ms, enabled=excluded.enabled, managed_by=excluded.managed_by`,
		device.ID, device.Name, device.ConnectionID, string(params), device.IntervalMs, enabled, device.ManagedBy,
	)
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM point WHERE device_id = ?", device.ID); err != nil {
		return fmt.Errorf("clear points: %w", err)
	}
	i := 0
	for _, point := range device.Points {
		if _, err := tx.Exec(
			"INSERT INTO point (device_id, name, address, data_type, scale, processing, seq) VALUES (?, ?, ?, ?, ?, ?, ?)",
			device.ID, point.Name, point.Address, string(point.DataType), point.Scale, marshalProcessing(point.Processing), i,
		); err != nil {
			return fmt.Errorf("insert point %q: %w", point.Name, err)
		}
		i++
	}
	return nil
}

// ---- 告警规则 ----

// SaveAlertRule 插入或更新告警规则(upsert);写入后通知热重载。
func (s *Store) SaveAlertRule(r model.AlertRule) error {
	refs, err := json.Marshal(r.ReferencedPoints)
	if err != nil {
		return fmt.Errorf("marshal referencedPoints: %w", err)
	}
	outputs, err := json.Marshal(r.OutputIDs)
	if err != nil {
		return fmt.Errorf("marshal outputIds: %w", err)
	}
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err = s.db.Exec(
		`INSERT INTO alert_rule (id, name, enabled, severity, expr, referenced_points, output_ids, freshness_seconds, cooldown_seconds, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, enabled=excluded.enabled, severity=excluded.severity,
		 expr=excluded.expr, referenced_points=excluded.referenced_points, output_ids=excluded.output_ids,
		 freshness_seconds=excluded.freshness_seconds, cooldown_seconds=excluded.cooldown_seconds, updated_at=excluded.updated_at`,
		r.ID, r.Name, enabled, r.Severity, r.Expr, string(refs), string(outputs), r.FreshnessSeconds, r.CooldownSeconds, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert alert rule: %w", err)
	}
	s.notify()
	return nil
}

func (s *Store) ListAlertRules() ([]model.AlertRule, error) {
	rows, err := s.db.Query(`SELECT id, name, enabled, severity, expr, referenced_points, output_ids, freshness_seconds, cooldown_seconds, created_at, updated_at FROM alert_rule ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query alert rules: %w", err)
	}
	defer rows.Close()
	rules := make([]model.AlertRule, 0)
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (s *Store) GetAlertRule(id string) (model.AlertRule, error) {
	row := s.db.QueryRow(`SELECT id, name, enabled, severity, expr, referenced_points, output_ids, freshness_seconds, cooldown_seconds, created_at, updated_at FROM alert_rule WHERE id = ?`, id)
	r, err := scanAlertRule(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.AlertRule{}, fmt.Errorf("alert rule %q not found", id)
		}
		return model.AlertRule{}, fmt.Errorf("get alert rule: %w", err)
	}
	return r, nil
}

func (s *Store) DeleteAlertRule(id string) error {
	if _, err := s.db.Exec("DELETE FROM alert_rule WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete alert rule: %w", err)
	}
	s.notify()
	return nil
}

func scanAlertRule(row rowScanner) (model.AlertRule, error) {
	var r model.AlertRule
	var enabled int
	var refs, outputs string
	if err := row.Scan(&r.ID, &r.Name, &enabled, &r.Severity, &r.Expr, &refs, &outputs, &r.FreshnessSeconds, &r.CooldownSeconds, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return model.AlertRule{}, fmt.Errorf("scan alert rule: %w", err)
	}
	r.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(refs), &r.ReferencedPoints); err != nil {
		return model.AlertRule{}, fmt.Errorf("unmarshal referencedPoints: %w", err)
	}
	if err := json.Unmarshal([]byte(outputs), &r.OutputIDs); err != nil {
		return model.AlertRule{}, fmt.Errorf("unmarshal outputIds: %w", err)
	}
	return r, nil
}

// ---- 告警记录 ----

// SaveAlert 写入一条已触发的告警记录(不通知:结果记录不影响运行态)。
func (s *Store) SaveAlert(a model.Alert) error {
	context, err := json.Marshal(a.Context)
	if err != nil {
		return fmt.Errorf("marshal alert context: %w", err)
	}
	var resolvedAt sql.NullString
	if a.ResolvedAt != nil {
		resolvedAt = sql.NullString{String: a.ResolvedAt.Format(time.RFC3339Nano), Valid: true}
	}
	_, err = s.db.Exec(
		`INSERT INTO alert (alert_id, rule_id, rule_name, severity, message, triggered_at, gateway_id, context, status, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.AlertID, a.RuleID, a.RuleName, a.Severity, a.Message, a.TriggeredAt.Format(time.RFC3339Nano), a.GatewayID, string(context), a.Status, &resolvedAt,
	)
	if err != nil {
		return fmt.Errorf("save alert: %w", err)
	}
	return nil
}

// ListAlerts 返回告警记录;status 非空时按状态过滤,按触发时间倒序。
func (s *Store) ListAlerts(status string) ([]model.Alert, error) {
	const base = `SELECT alert_id, rule_id, rule_name, severity, message, triggered_at, gateway_id, context, status, resolved_at FROM alert`
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.db.Query(base+" WHERE status = ? ORDER BY triggered_at DESC", status)
	} else {
		rows, err = s.db.Query(base + " ORDER BY triggered_at DESC")
	}
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer rows.Close()
	alerts := make([]model.Alert, 0)
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

// UpdateAlertStatus 更新告警状态(解除时置 resolved 并记解除时间)。
func (s *Store) UpdateAlertStatus(alertID, status string, resolvedAt time.Time) error {
	var resolved sql.NullString
	if status == string(model.AlertResolved) {
		resolved = sql.NullString{String: resolvedAt.Format(time.RFC3339Nano), Valid: true}
	}
	_, err := s.db.Exec(
		`UPDATE alert SET status = ?, resolved_at = ? WHERE alert_id = ?`,
		status, &resolved, alertID,
	)
	if err != nil {
		return fmt.Errorf("update alert status: %w", err)
	}
	return nil
}

func scanAlert(row rowScanner) (model.Alert, error) {
	var a model.Alert
	var context, triggeredAt string
	var resolvedAt sql.NullString
	if err := row.Scan(&a.AlertID, &a.RuleID, &a.RuleName, &a.Severity, &a.Message, &triggeredAt, &a.GatewayID, &context, &a.Status, &resolvedAt); err != nil {
		return model.Alert{}, fmt.Errorf("scan alert: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, triggeredAt)
	if err != nil {
		return model.Alert{}, fmt.Errorf("parse triggered_at: %w", err)
	}
	a.TriggeredAt = t
	if err := json.Unmarshal([]byte(context), &a.Context); err != nil {
		return model.Alert{}, fmt.Errorf("unmarshal alert context: %w", err)
	}
	if resolvedAt.Valid && resolvedAt.String != "" {
		if rt, err := time.Parse(time.RFC3339Nano, resolvedAt.String); err == nil {
			a.ResolvedAt = &rt
		}
	}
	return a, nil
}
