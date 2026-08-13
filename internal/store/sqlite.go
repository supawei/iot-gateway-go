package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"

	"iot-gateway-go/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS device (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    driver      TEXT NOT NULL,
    connection  TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS point (
    device_id   TEXT NOT NULL,
    name        TEXT NOT NULL,
    address     TEXT NOT NULL,
    data_type   TEXT NOT NULL,
    interval_ms INTEGER NOT NULL,
    scale       REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (device_id, name),
    FOREIGN KEY (device_id) REFERENCES device(id) ON DELETE CASCADE
);`

// Store 负责设备/点位配置的持久化与变更通知。
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

func (s *Store) ListDevices() ([]model.Device, error) {
	devices, err := s.queryDevices("SELECT id, name, driver, connection, enabled FROM device")
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
	devices, err := s.queryDevices("SELECT id, name, driver, connection, enabled FROM device WHERE id = ?", id)
	if err != nil {
		return model.Device{}, err
	}
	if len(devices) == 0 {
		return model.Device{}, fmt.Errorf("device %q not found", id)
	}
	device := devices[0]
	device.Points, err = s.queryPoints("SELECT name, address, data_type, interval_ms, scale FROM point WHERE device_id = ?", id)
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
		"INSERT INTO point (device_id, name, address, data_type, interval_ms, scale) VALUES (?, ?, ?, ?, ?, ?)",
		deviceID, point.Name, point.Address, string(point.DataType), point.IntervalMs, point.Scale,
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
	rows, err := s.db.Query("SELECT device_id, name, address, data_type, interval_ms, scale FROM point")
	if err != nil {
		return nil, fmt.Errorf("query all points: %w", err)
	}
	defer rows.Close()
	result := make(map[string][]model.Point)
	for rows.Next() {
		var deviceID, dataType string
		var point model.Point
		if err := rows.Scan(&deviceID, &point.Name, &point.Address, &dataType, &point.IntervalMs, &point.Scale); err != nil {
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

func scanDevice(row rowScanner) (model.Device, error) {
	var device model.Device
	var connection string
	var enabled int
	if err := row.Scan(&device.ID, &device.Name, &device.Driver, &connection, &enabled); err != nil {
		return model.Device{}, fmt.Errorf("scan device: %w", err)
	}
	device.Connection = json.RawMessage(connection)
	device.Enabled = enabled != 0
	return device, nil
}

func scanPoint(row rowScanner) (model.Point, error) {
	var point model.Point
	var dataType string
	if err := row.Scan(&point.Name, &point.Address, &dataType, &point.IntervalMs, &point.Scale); err != nil {
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
	_, err := tx.Exec(
		`INSERT INTO device (id, name, driver, connection, enabled) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, driver=excluded.driver, connection=excluded.connection, enabled=excluded.enabled`,
		device.ID, device.Name, device.Driver, string(device.Connection), enabled,
	)
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM point WHERE device_id = ?", device.ID); err != nil {
		return fmt.Errorf("clear points: %w", err)
	}
	for _, point := range device.Points {
		if _, err := tx.Exec(
			"INSERT INTO point (device_id, name, address, data_type, interval_ms, scale) VALUES (?, ?, ?, ?, ?, ?)",
			device.ID, point.Name, point.Address, string(point.DataType), point.IntervalMs, point.Scale,
		); err != nil {
			return fmt.Errorf("insert point %q: %w", point.Name, err)
		}
	}
	return nil
}
