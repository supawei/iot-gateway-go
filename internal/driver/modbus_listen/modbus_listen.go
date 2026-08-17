// Package modbus_listen 实现"监听型"南向驱动:网关作为 TCP 服务端被动 listen,
// Modbus 设备(或上游主站)主动连入并上报 Modbus TCP 帧,驱动按从机地址(UnitID)
// 路由到已配置设备并解码推送。它是 driver.Listener 能力的一个参考实现,与
// driver.Subscriber(网关主动订阅)相对,验证了 Core 对监听类协议的扩展点。
package modbus_listen

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

func init() {
	driver.Register("modbus_listen", &modbusListenDriver{pool: make(map[string]*sharedListener)})
}

// modbusListenDriver 维护按 ConnectionID 复用的监听器池:同一监听地址上的多个
// 从机设备共享一个 TCP listener,按 UnitID 分派。
type modbusListenDriver struct {
	mu   sync.Mutex
	pool map[string]*sharedListener
}

// EndpointKey 归一化物理端点(监听地址):两个连接监听同一地址会 bind 冲突,保存时拒绝。
func (*modbusListenDriver) EndpointKey(connection json.RawMessage) string {
	cfg, err := parseConnConfig(connection)
	if err != nil {
		return ""
	}
	return "listen|" + cfg.Listen
}

// ConfigSchema 声明 Connection.config 结构。
func (*modbusListenDriver) ConfigSchema() []driver.Field {
	return []driver.Field{
		{Name: "listen", Label: "监听地址", Type: driver.FieldString, Required: true, Default: ":502", Placeholder: ":502"},
		{Name: "timeout", Label: "读超时", Type: driver.FieldString, Default: "60s", Hint: "设备连接空闲超时,超时断开需重连"},
	}
}

// ParamSchema 声明 Device.params 结构。
func (*modbusListenDriver) ParamSchema() []driver.Field {
	return []driver.Field{
		{Name: "slaveId", Label: "从机地址", Type: driver.FieldInt, Default: 1, Hint: "上报帧的 UnitID,用于路由到本设备"},
	}
}

// sharedListener 是一个共享的 TCP 监听 socket,引用计数管理生命周期。devices 以
// UnitID 为 key 记录"从机 -> 设备点位 + 回调",accept 到连接后按帧头 UnitID 路由。
type sharedListener struct {
	connectionID string
	addr         string
	timeout      time.Duration
	refCount     int

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	devices map[byte]*listenDevice
	started bool
}

// listenDevice 是监听 socket 上一个已注册的从机设备:帧头 UnitID 命中后,按其
// 点位表解码并回调 onData 推送 DataPoint。
type listenDevice struct {
	deviceID string
	slaveID  byte
	points   []model.Point
	onData   func(model.DataPoint)
}

// listenConn 是监听模式下的设备连接:内嵌 Read/Close 以满足 Conn 接口,额外实现
// driver.Listener。Open 仅在驱动为 modbus_listen 时返回此类型。
type listenConn struct {
	deviceID string
	slaveID  byte
	shared   *sharedListener
	driver   *modbusListenDriver
}

// Open 解析监听地址(ConnConfig)与从机地址(DeviceParams),按 ConnectionID 取得
// 共享监听器;不发起任何主动连接——真正的 listen 在首次 Listen 时惰性建立。
func (d *modbusListenDriver) Open(ctx context.Context, req driver.OpenRequest) (driver.Conn, error) {
	cfg, err := parseConnConfig(req.ConnConfig)
	if err != nil {
		return nil, err
	}
	params, err := parseDeviceParams(req.DeviceParams)
	if err != nil {
		return nil, err
	}
	shared, err := d.acquire(req.ConnectionID, cfg)
	if err != nil {
		return nil, err
	}
	return &listenConn{
		deviceID: req.DeviceID,
		slaveID:  params.SlaveID,
		shared:   shared,
		driver:   d,
	}, nil
}

func (d *modbusListenDriver) acquire(connectionID string, cfg connConfig) (*sharedListener, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if shared, ok := d.pool[connectionID]; ok {
		shared.refCount++
		return shared, nil
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	shared := &sharedListener{
		connectionID: connectionID,
		addr:         cfg.Listen,
		timeout:      timeout,
		refCount:     1,
		ctx:          ctx,
		cancel:       cancel,
	}
	d.pool[connectionID] = shared
	return shared, nil
}

func (d *modbusListenDriver) release(shared *sharedListener) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	shared.refCount--
	if shared.refCount > 0 {
		return nil
	}
	delete(d.pool, shared.connectionID)
	// 取消 ctx 触发 serve goroutine 关闭监听 socket,accept 立即返回。
	if shared.cancel != nil {
		shared.cancel()
	}
	return nil
}

// Read 在监听模式下无意义(数据由设备推送),仅满足 Conn 接口;正常情况下不会被
// scheduler 调用(检测到 Listener 能力后走 Listen 分支)。
func (c *listenConn) Read(_ context.Context, _ []model.Point) ([]model.DataPoint, error) {
	return nil, errors.New("modbus_listen: data is pushed by devices, not polled")
}

func (c *listenConn) Close() error {
	return c.driver.release(c.shared)
}

// Listen 把设备点位登记进共享监听器:按从机地址注册路由,首次调用时惰性建立
// 监听 socket 并启动 accept 循环。
func (c *listenConn) Listen(ctx context.Context, points []model.Point, onData func(model.DataPoint)) error {
	return c.shared.register(c.slaveID, &listenDevice{
		deviceID: c.deviceID,
		slaveID:  c.slaveID,
		points:   points,
		onData:   onData,
	})
}

func (s *sharedListener) register(slaveID byte, dev *listenDevice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.devices == nil {
		s.devices = make(map[byte]*listenDevice)
	}
	s.devices[slaveID] = dev
	if s.started {
		return nil
	}
	// 同步 bind:失败立即返回错误,由 scheduler 标记设备离线并记录,不再静默卡死;
	// 端口占用等场景会在下一次 reload 重试。
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	s.started = true
	go s.serveLoop(ln)
	return nil
}

// serveLoop 循环 accept;每个连接交给独立 goroutine 读帧。ctx 取消时关闭 listener
// 使 accept 退出。单个设备连接靠读超时回收,避免僵尸 goroutine。
func (s *sharedListener) serveLoop(ln net.Listener) {
	slog.Info("modbus_listen listening", "addr", s.addr, "connection", s.connectionID)
	go func() {
		<-s.ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

// handleConn 在一个设备连接上循环读 Modbus TCP 帧并分派;读错误或超时即关闭连接。
func (s *sharedListener) handleConn(conn net.Conn) {
	defer conn.Close()
	for {
		if s.timeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.timeout))
		}
		frame, err := readFrame(conn)
		if err != nil {
			return
		}
		s.dispatch(frame)
	}
}

// dispatch 解析帧头 UnitID 并路由到对应设备,解码后推送。未知 UnitID 静默丢弃。
func (s *sharedListener) dispatch(frame []byte) {
	if len(frame) < 9 {
		return
	}
	unitID := frame[6]
	byteCount := int(frame[8])
	data := frame[9:]
	if len(data) < byteCount {
		return
	}
	s.mu.Lock()
	dev := s.devices[unitID]
	s.mu.Unlock()
	if dev == nil {
		return
	}
	dev.deliver(registers(data[:byteCount]))
}

// readFrame 按 Modbus TCP MBAP 定长读一帧:6 字节头(txid/proto/length)+ length 字节
// (UnitID + PDU)。length < 2 视为非法帧。
func readFrame(conn net.Conn) ([]byte, error) {
	var header [6]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(header[4:6]))
	if length < 2 {
		return nil, fmt.Errorf("modbus_listen: mbap length too short: %d", length)
	}
	frame := make([]byte, 6+length)
	copy(frame, header[:])
	if _, err := io.ReadFull(conn, frame[6:]); err != nil {
		return nil, err
	}
	return frame, nil
}

// registers 把 PDU 数据段字节流按大端解析成 16 位寄存器数组。
func registers(data []byte) []uint16 {
	regs := make([]uint16, len(data)/2)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(data[i*2:])
	}
	return regs
}

// deliver 按本设备点位表从寄存器数组解码并回调。时间戳统一取当前时刻(监听场景
// 无轮询周期,数据到达即当前);单点越界/类型不支持标 uncertain,不阻断同批。
func (d *listenDevice) deliver(regs []uint16) {
	now := time.Now()
	for _, point := range d.points {
		dp := model.DataPoint{
			DeviceID: d.deviceID, Point: point.Name,
			Timestamp: now, Quality: model.QualityBad,
		}
		value, ok := decodePoint(point, regs)
		if !ok {
			dp.Quality = model.QualityUncertain
		} else {
			dp.Value = applyScale(value, point.Scale, point.DataType)
			dp.Quality = model.QualityGood
		}
		d.onData(dp)
	}
}

// decodePoint 按点位地址(寄存器偏移)与类型从寄存器数组解码。int32/uint32/float32
// 占 2 个寄存器(大端,与 modbus 轮询驱动一致)。越界返回 ok=false。
func decodePoint(point model.Point, regs []uint16) (interface{}, bool) {
	offset, err := strconv.Atoi(point.Address)
	if err != nil || offset < 0 {
		return nil, false
	}
	qty := registerCount(point.DataType)
	if offset+qty > len(regs) {
		return nil, false
	}
	switch point.DataType {
	case model.DataTypeBool:
		return regs[offset] != 0, true
	case model.DataTypeInt16:
		return int16(regs[offset]), true
	case model.DataTypeUInt16:
		return regs[offset], true
	case model.DataTypeInt32:
		return int32(uint32(regs[offset])<<16 | uint32(regs[offset+1])), true
	case model.DataTypeUInt32:
		return uint32(regs[offset])<<16 | uint32(regs[offset+1]), true
	case model.DataTypeFloat:
		return math.Float32frombits(uint32(regs[offset])<<16 | uint32(regs[offset+1])), true
	default:
		return nil, false
	}
}

func registerCount(dt model.DataType) int {
	switch dt {
	case model.DataTypeInt32, model.DataTypeUInt32, model.DataTypeFloat:
		return 2
	default:
		return 1
	}
}

// applyScale 缩放原始值;bool 不缩放,数值类型缩放后统一为 float64。
func applyScale(value interface{}, scale float64, dt model.DataType) interface{} {
	if scale == 0 || dt == model.DataTypeBool {
		return value
	}
	switch v := value.(type) {
	case int16:
		return float64(v) * scale
	case uint16:
		return float64(v) * scale
	case int32:
		return float64(v) * scale
	case uint32:
		return float64(v) * scale
	case float32:
		return float64(v) * scale
	default:
		return value
	}
}

const defaultTimeout = 60 * time.Second

// connConfig 是监听传输参数:listen 为本地监听地址(":502" 或 "0.0.0.0:502"),
// timeout 为设备连接的空闲读超时(超时即断开,设备需重连)。
type connConfig struct {
	Listen  string `json:"listen"`
	Timeout string `json:"timeout"`
}

func parseConnConfig(raw json.RawMessage) (connConfig, error) {
	cfg := connConfig{Timeout: defaultTimeout.String()}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return connConfig{}, fmt.Errorf("parse modbus_listen connection: %w", err)
		}
	}
	if cfg.Listen == "" {
		return connConfig{}, errors.New("modbus_listen listen address is required")
	}
	if cfg.Timeout == "" {
		cfg.Timeout = defaultTimeout.String()
	}
	if _, err := time.ParseDuration(cfg.Timeout); err != nil {
		return connConfig{}, fmt.Errorf("invalid timeout %q: %w", cfg.Timeout, err)
	}
	return cfg, nil
}

// deviceParams 是设备级路由参数:slaveId 为 Modbus 从机地址(UnitID),用于把上报帧
// 路由到对应设备。
type deviceParams struct {
	SlaveID byte `json:"slaveId"`
}

func parseDeviceParams(raw json.RawMessage) (deviceParams, error) {
	params := deviceParams{}
	if len(raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return deviceParams{}, fmt.Errorf("parse modbus_listen device params: %w", err)
	}
	return params, nil
}
