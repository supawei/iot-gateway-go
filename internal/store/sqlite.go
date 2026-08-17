package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"iot-gateway-go/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS connection (
    id     TEXT PRIMARY KEY,
    name   TEXT NOT NULL,
    driver TEXT NOT NULL,
    config TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS device (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    params        TEXT NOT NULL DEFAULT '{}',
    interval_ms   INTEGER NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    FOREIGN KEY (connection_id) REFERENCES connection(id) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS point (
    device_id   TEXT NOT NULL,
    name        TEXT NOT NULL,
    address     TEXT NOT NULL,
    data_type   TEXT NOT NULL,
    scale       REAL NOT NULL DEFAULT 0,
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

// Store 负责连接/设备/点位配置的持久化与变更通知。
type Store struct {
	db       *sql.DB
	changeCh chan struct{}
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
	// 预置默认网关设置(幂等):数据库为空时内置默认网关 ID。
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`,
		SettingGatewayID, DefaultGatewayID,
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrap gateway settings: %w", err)
	}
	return &Store{db: db, changeCh: make(chan struct{}, 1)}, nil
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

// OnChange 返回配置变更信号 channel;写入后非阻塞通知,scheduler 据此热加载。
func (s *Store) OnChange() <-chan struct{} {
	return s.changeCh
}

func (s *Store) notify() {
	select {
	case s.changeCh <- struct{}{}:
	default: // 已有待处理信号则丢弃,避免堆积
	}
}

func (s *Store) SaveConnection(conn model.Connection) error {
	_, err := s.db.Exec(
		`INSERT INTO connection (id, name, driver, config) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, driver=excluded.driver, config=excluded.config`,
		conn.ID, conn.Name, conn.Driver, string(conn.Config),
	)
	if err != nil {
		return fmt.Errorf("upsert connection: %w", err)
	}
	s.notify()
	return nil
}

func (s *Store) ListConnections() ([]model.Connection, error) {
	rows, err := s.db.Query("SELECT id, name, driver, config FROM connection")
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
	row := s.db.QueryRow("SELECT id, name, driver, config FROM connection WHERE id = ?", id)
	conn, err := scanConnection(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Connection{}, fmt.Errorf("connection %q not found", id)
		}
		return model.Connection{}, fmt.Errorf("get connection: %w", err)
	}
	return conn, nil
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
	devices, err := s.queryDevices("SELECT id, name, connection_id, params, interval_ms, enabled FROM device")
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
	devices, err := s.queryDevices("SELECT id, name, connection_id, params, interval_ms, enabled FROM device WHERE id = ?", id)
	if err != nil {
		return model.Device{}, err
	}
	if len(devices) == 0 {
		return model.Device{}, fmt.Errorf("device %q not found", id)
	}
	device := devices[0]
	device.Points, err = s.queryPoints("SELECT name, address, data_type, scale FROM point WHERE device_id = ?", id)
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

func (s *Store) AddPoint(deviceID string, point model.Point) error {
	_, err := s.db.Exec(
		"INSERT INTO point (device_id, name, address, data_type, scale) VALUES (?, ?, ?, ?, ?)",
		deviceID, point.Name, point.Address, string(point.DataType), point.Scale,
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
	rows, err := s.db.Query("SELECT device_id, name, address, data_type, scale FROM point")
	if err != nil {
		return nil, fmt.Errorf("query all points: %w", err)
	}
	defer rows.Close()
	result := make(map[string][]model.Point)
	for rows.Next() {
		var deviceID, dataType string
		var point model.Point
		if err := rows.Scan(&deviceID, &point.Name, &point.Address, &dataType, &point.Scale); err != nil {
			return nil, fmt.Errorf("scan point: %w", err)
		}
		point.DataType = model.DataType(dataType)
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
	if err := row.Scan(&conn.ID, &conn.Name, &conn.Driver, &config); err != nil {
		return model.Connection{}, fmt.Errorf("scan connection: %w", err)
	}
	conn.Config = json.RawMessage(config)
	return conn, nil
}

func scanDevice(row rowScanner) (model.Device, error) {
	var device model.Device
	var connectionID, params string
	var enabled int
	if err := row.Scan(&device.ID, &device.Name, &connectionID, &params, &device.IntervalMs, &enabled); err != nil {
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
	if err := row.Scan(&point.Name, &point.Address, &dataType, &point.Scale); err != nil {
		return model.Point{}, fmt.Errorf("scan point: %w", err)
	}
	point.DataType = model.DataType(dataType)
	return point, nil
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
		`INSERT INTO device (id, name, connection_id, params, interval_ms, enabled) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, connection_id=excluded.connection_id, params=excluded.params, interval_ms=excluded.interval_ms, enabled=excluded.enabled`,
		device.ID, device.Name, device.ConnectionID, string(params), device.IntervalMs, enabled,
	)
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM point WHERE device_id = ?", device.ID); err != nil {
		return fmt.Errorf("clear points: %w", err)
	}
	for _, point := range device.Points {
		if _, err := tx.Exec(
			"INSERT INTO point (device_id, name, address, data_type, scale) VALUES (?, ?, ?, ?, ?)",
			device.ID, point.Name, point.Address, string(point.DataType), point.Scale,
		); err != nil {
			return fmt.Errorf("insert point %q: %w", point.Name, err)
		}
	}
	return nil
}
