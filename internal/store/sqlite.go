package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
);`

// ErrConnectionInUse 表示连接仍被设备引用,不可删除。
var ErrConnectionInUse = errors.New("connection is referenced by devices")

// Store 负责连接/设备/点位配置的持久化与变更通知。
type Store struct {
	db       *sql.DB
	changeCh chan struct{}
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db, changeCh: make(chan struct{}, 1)}, nil
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
	var conns []model.Connection
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
		devices[index].Points = pointsByDevice[devices[index].ID]
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

func (s *Store) queryDevices(query string, args ...any) ([]model.Device, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	defer rows.Close()
	var devices []model.Device
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
	var points []model.Point
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
