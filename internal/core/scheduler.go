package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

const defaultInterval = 5 * time.Second

// Scheduler 用 cron 统一调度采集任务,任务投递到 worker pool 执行。
// 常驻 goroutine = cron 调度器 + poolSize 个 worker,与设备数解耦;
// 配置变更时全量重载(停旧调度与 pool、按新配置重建)。
type Scheduler struct {
	store         *store.Store
	output        chan<- model.DataPoint
	poolSize      int
	baseCtx       context.Context
	mu            sync.Mutex
	cron          *cron.Cron
	conns         []driver.Conn
	taskCh        chan collectTask
	workersDone   <-chan struct{}
	collectCancel context.CancelFunc
}

func NewScheduler(st *store.Store, dataPoints chan<- model.DataPoint, poolSize int) *Scheduler {
	if poolSize <= 0 {
		poolSize = 16
	}
	return &Scheduler{store: st, output: dataPoints, poolSize: poolSize}
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
				slog.Error("scheduler reload failed", "err", err)
			}
		}
	}
}

func (s *Scheduler) reload() error {
	s.stopCollectors()

	collectCtx, cancel := context.WithCancel(s.baseCtx)
	taskCh := make(chan collectTask, s.poolSize)
	s.mu.Lock()
	s.collectCancel = cancel
	s.taskCh = taskCh
	s.workersDone = s.startWorkers(taskCh)
	s.mu.Unlock()

	c := cron.New()
	devices, err := s.store.ListDevices()
	if err != nil {
		slog.Error("list devices failed", "err", err)
		s.stopCollectors()
		return err
	}
	for _, device := range devices {
		if !device.Enabled {
			continue
		}
		conn, ok := s.openDevice(collectCtx, device)
		if !ok || len(device.Points) == 0 {
			continue
		}
		job := &deviceJob{
			taskCh:   taskCh,
			conn:     conn,
			points:   device.Points,
			deviceID: device.ID,
			ctx:      collectCtx,
		}
		if _, err := c.AddJob(intervalSpec(device.IntervalMs), job); err != nil {
			slog.Error("add cron job failed", "device", device.ID, "err", err)
		}
	}

	s.mu.Lock()
	s.cron = c
	s.mu.Unlock()
	c.Start()
	return nil
}

// stopCollectors 按"cancel Read -> 停 cron -> 关 taskCh -> 等 workers 退出 -> Close conns"顺序清理,
// 保证关连接时无并发 Read:cron.Stop 后无 job 投递,workers 退出后无 Read 在执行。
func (s *Scheduler) stopCollectors() {
	s.mu.Lock()
	cancel := s.collectCancel
	c := s.cron
	conns := s.conns
	taskCh := s.taskCh
	workersDone := s.workersDone
	s.collectCancel = nil
	s.cron = nil
	s.conns = nil
	s.taskCh = nil
	s.workersDone = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	if c != nil {
		<-c.Stop().Done()
	}
	if taskCh != nil {
		close(taskCh)
	}
	if workersDone != nil {
		<-workersDone
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			slog.Error("close device connection failed", "err", err)
		}
	}
}

func (s *Scheduler) openDevice(ctx context.Context, device model.Device) (driver.Conn, bool) {
	connection, err := s.store.GetConnection(device.ConnectionID)
	if err != nil {
		slog.Error("get connection failed", "device", device.ID, "connection", device.ConnectionID, "err", err)
		return nil, false
	}
	drv, err := driver.Get(connection.Driver)
	if err != nil {
		slog.Error("driver not registered", "device", device.ID, "driver", connection.Driver, "err", err)
		return nil, false
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     device.ID,
		ConnectionID: device.ConnectionID,
		ConnConfig:   connection.Config,
		DeviceParams: device.Params,
	})
	if err != nil {
		slog.Error("open device failed", "device", device.ID, "err", err)
		return nil, false
	}
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
	return conn, true
}

func (s *Scheduler) startWorkers(taskCh <-chan collectTask) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(s.poolSize)
	for workerIndex := 0; workerIndex < s.poolSize; workerIndex++ {
		go func() {
			defer wg.Done()
			for task := range taskCh {
				s.collectOnce(task.ctx, task.conn, task.points)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

func (s *Scheduler) collectOnce(ctx context.Context, conn driver.Conn, points []model.Point) {
	dataPoints, err := conn.Read(ctx, points)
	if err != nil {
		// 配置级错误(整批无效):跳过本次,不发送
		slog.Error("read points failed", "points", len(points), "err", err)
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

type collectTask struct {
	ctx    context.Context
	conn   driver.Conn
	points []model.Point
}

type deviceJob struct {
	taskCh   chan collectTask
	conn     driver.Conn
	points   []model.Point
	deviceID string
	ctx      context.Context
}

// Run 到达采集周期时触发:把采集任务非阻塞投递到 pool,满则跳过本次(防堆积)。
func (j *deviceJob) Run() {
	select {
	case j.taskCh <- collectTask{ctx: j.ctx, conn: j.conn, points: j.points}:
	default:
		slog.Warn("pool busy, skip collect", "device", j.deviceID)
	}
}

func intervalSpec(intervalMs int) string {
	interval := time.Duration(intervalMs) * time.Millisecond
	if interval <= 0 {
		interval = defaultInterval
	}
	return "@every " + interval.String()
}
