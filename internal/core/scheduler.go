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

// Scheduler 按各点位 Interval 周期采集设备,产 DataPoint 到 output channel;
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
	drv, err := driver.Get(device.Driver)
	if err != nil {
		log.Printf("device %q: driver %q not registered: %v", device.ID, device.Driver, err)
		return
	}
	conn, err := drv.Open(device.ID, device.Connection)
	if err != nil {
		log.Printf("device %q: open failed: %v", device.ID, err)
		return
	}
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
	for _, point := range device.Points {
		s.startPoint(ctx, conn, point)
	}
}

func (s *Scheduler) startPoint(ctx context.Context, conn driver.Conn, point model.Point) {
	interval := time.Duration(point.IntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = defaultInterval
	}
	go s.collectLoop(ctx, conn, point, interval)
}

func (s *Scheduler) collectLoop(ctx context.Context, conn driver.Conn, point model.Point, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.collectOnce(ctx, conn, point)
		}
	}
}

func (s *Scheduler) collectOnce(ctx context.Context, conn driver.Conn, point model.Point) {
	dataPoint, err := conn.Read(point)
	if err != nil {
		// 配置级错误(地址无法解析等):数据点无效,跳过不发
		log.Printf("read point %q failed: %v", point.Name, err)
		return
	}
	select {
	case s.output <- dataPoint:
	case <-ctx.Done():
	}
}
