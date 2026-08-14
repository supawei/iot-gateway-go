package core

import (
	"context"
	"log"
	"sync"
	"time"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

const defaultInterval = 5 * time.Second

// Scheduler 按设备级 Interval 周期批量采集,产 DataPoint 到 output channel;
// 配置变更时全量重载(停旧采集、按新配置重启),MVP 暂不增量 diff。
type Scheduler struct {
	store         *store.Store
	output        chan<- model.DataPoint
	baseCtx       context.Context
	mu            sync.Mutex
	collectCancel context.CancelFunc
	conns         []driver.Conn
}

func NewScheduler(st *store.Store, dataPoints chan<- model.DataPoint) *Scheduler {
	return &Scheduler{store: st, output: dataPoints}
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.baseCtx = ctx
	if err := s.reload(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			s.stopCollectors()
			return ctx.Err()
		case <-s.store.OnChange():
			if err := s.reload(); err != nil {
				log.Printf("scheduler reload failed: %v", err)
			}
		}
	}
}

func (s *Scheduler) reload() error {
	s.stopCollectors()
	collectCtx, cancel := context.WithCancel(s.baseCtx)
	s.mu.Lock()
	s.collectCancel = cancel
	s.mu.Unlock()
	devices, err := s.store.ListDevices()
	if err != nil {
		cancel()
		return err
	}
	for _, device := range devices {
		if device.Enabled {
			s.startDevice(collectCtx, device)
		}
	}
	return nil
}

func (s *Scheduler) stopCollectors() {
	s.mu.Lock()
	cancel := s.collectCancel
	conns := s.conns
	s.collectCancel = nil
	s.conns = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			log.Printf("close device connection failed: %v", err)
		}
	}
}

func (s *Scheduler) startDevice(ctx context.Context, device model.Device) {
	connection, err := s.store.GetConnection(device.ConnectionID)
	if err != nil {
		log.Printf("device %q: get connection %q failed: %v", device.ID, device.ConnectionID, err)
		return
	}
	drv, err := driver.Get(connection.Driver)
	if err != nil {
		log.Printf("device %q: driver %q not registered: %v", device.ID, connection.Driver, err)
		return
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     device.ID,
		ConnectionID: device.ConnectionID,
		ConnConfig:   connection.Config,
		DeviceParams: device.Params,
	})
	if err != nil {
		log.Printf("device %q: open failed: %v", device.ID, err)
		return
	}
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
	if len(device.Points) == 0 {
		return
	}
	// 每设备一个采集循环,按设备级 Interval 批量连读所有点位
	go s.collectLoop(ctx, conn, device.Points, device.IntervalMs)
}

func (s *Scheduler) collectLoop(ctx context.Context, conn driver.Conn, points []model.Point, intervalMs int) {
	interval := time.Duration(intervalMs) * time.Millisecond
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.collectOnce(ctx, conn, points)
		}
	}
}

func (s *Scheduler) collectOnce(ctx context.Context, conn driver.Conn, points []model.Point) {
	dataPoints, err := conn.Read(ctx, points)
	if err != nil {
		// 配置级错误(整批无效):跳过本次,不发送
		log.Printf("read %d points failed: %v", len(points), err)
		return
	}
	for _, dp := range dataPoints {
		select {
		case s.output <- dp:
		case <-ctx.Done():
			return
		}
	}
}
