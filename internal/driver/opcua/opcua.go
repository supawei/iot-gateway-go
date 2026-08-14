package opcua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

func init() {
	driver.Register("opcua", &opcuaDriver{pool: make(map[string]*sharedSession)})
}

type opcuaDriver struct {
	mu   sync.Mutex
	pool map[string]*sharedSession
}

// sharedSession 是按 ConnectionID 共享的 OPC UA client/session,引用计数管理生命周期。
// OPC UA 走 TCP 全双工且 gopcua Client 支持并发请求,故同连接多设备可并发 Read,无需串行化。
type sharedSession struct {
	connectionID string
	client       *opcua.Client
	refCount     int
}

type opcuaConn struct {
	deviceID string
	shared   *sharedSession
	driver   *opcuaDriver
}

func (d *opcuaDriver) Open(ctx context.Context, req driver.OpenRequest) (driver.Conn, error) {
	cfg, err := parseConnConfig(req.ConnConfig)
	if err != nil {
		return nil, err
	}
	shared, err := d.acquire(ctx, req.ConnectionID, cfg)
	if err != nil {
		return nil, err
	}
	return &opcuaConn{deviceID: req.DeviceID, shared: shared, driver: d}, nil
}

func (d *opcuaDriver) acquire(ctx context.Context, connectionID string, cfg connConfig) (*sharedSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if shared, ok := d.pool[connectionID]; ok {
		shared.refCount++
		return shared, nil
	}
	client, err := buildClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	shared := &sharedSession{connectionID: connectionID, client: client, refCount: 1}
	d.pool[connectionID] = shared
	return shared, nil
}

func (d *opcuaDriver) release(shared *sharedSession) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	shared.refCount--
	if shared.refCount > 0 {
		return nil
	}
	delete(d.pool, shared.connectionID)
	return shared.client.Close(context.Background())
}

func (c *opcuaConn) Read(ctx context.Context, points []model.Point) ([]model.DataPoint, error) {
	results := make([]model.DataPoint, len(points))
	now := time.Now()
	for index, point := range points {
		results[index] = model.DataPoint{
			DeviceID: c.deviceID, Point: point.Name,
			Timestamp: now, Quality: model.QualityBad,
		}
	}
	nodes, indices := planReads(points)
	if len(nodes) == 0 {
		return results, nil
	}
	resp, err := c.shared.client.Read(ctx, &ua.ReadRequest{NodesToRead: nodes})
	if err != nil {
		return results, err
	}
	applyReadResults(results, indices, resp.Results, points)
	return results, nil
}

func (c *opcuaConn) Close() error {
	return c.driver.release(c.shared)
}

// Write 下发点位值:解析 NodeID 后构造批量 WriteRequest,按 server 返回 status 标记各点。
// 单点 NodeID 解析失败或类型不匹配标记 Ok=false,不阻断同批。
func (c *opcuaConn) Write(ctx context.Context, items []model.WriteItem) ([]driver.WriteResult, error) {
	results := make([]driver.WriteResult, len(items))
	var nodes []*ua.WriteValue
	var indices []int
	for index, item := range items {
		results[index] = driver.WriteResult{Point: item.Point.Name}
		nodeID, err := ua.ParseNodeID(item.Point.Address)
		if err != nil {
			continue
		}
		val, ok := encodeValue(item.Value, item.Point.DataType)
		if !ok {
			continue
		}
		variant, err := ua.NewVariant(val)
		if err != nil {
			continue
		}
		nodes = append(nodes, &ua.WriteValue{
			NodeID:      nodeID,
			AttributeID: ua.AttributeIDValue,
			Value:       &ua.DataValue{Value: variant},
		})
		indices = append(indices, index)
	}
	if len(nodes) == 0 {
		return results, nil
	}
	resp, err := c.shared.client.Write(ctx, &ua.WriteRequest{NodesToWrite: nodes})
	if err != nil {
		return results, err
	}
	for row, status := range resp.Results {
		if row >= len(indices) {
			break
		}
		if status == ua.StatusOK {
			results[indices[row]].Ok = true
		}
	}
	return results, nil
}

// planReads 解析每个点位的 NodeID 地址;解析失败的点跳过(保持 bad),成功的收集为批量读请求。
func planReads(points []model.Point) ([]*ua.ReadValueID, []int) {
	var nodes []*ua.ReadValueID
	var indices []int
	for index, point := range points {
		nodeID, err := ua.ParseNodeID(point.Address)
		if err != nil {
			continue
		}
		nodes = append(nodes, &ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue})
		indices = append(indices, index)
	}
	return nodes, indices
}

func applyReadResults(results []model.DataPoint, indices []int, readResults []*ua.DataValue, points []model.Point) {
	for row, value := range readResults {
		if row >= len(indices) {
			break
		}
		index := indices[row]
		if value.Status != ua.StatusOK {
			continue
		}
		decoded, ok := decodeValue(value.Value.Value(), points[index].DataType, points[index].Scale)
		if !ok {
			results[index].Quality = model.QualityUncertain
			continue
		}
		results[index].Value = decoded
		results[index].Quality = model.QualityGood
	}
}

// decodeValue 把 OPC UA variant 返回的 Go 原生值按声明类型校验,并应用缩放。
// 类型不匹配返回 ok=false(标记 uncertain),而非整批失败。
func decodeValue(raw interface{}, dataType model.DataType, scale float64) (interface{}, bool) {
	switch dataType {
	case model.DataTypeBool:
		value, ok := raw.(bool)
		return value, ok
	case model.DataTypeString:
		value, ok := raw.(string)
		return value, ok
	case model.DataTypeInt16, model.DataTypeUInt16, model.DataTypeInt32, model.DataTypeUInt32, model.DataTypeInt64:
		value, ok := toInt64(raw)
		if !ok {
			return nil, false
		}
		if scale != 0 {
			return float64(value) * scale, true
		}
		return value, true
	case model.DataTypeFloat, model.DataTypeDouble:
		value, ok := toFloat64(raw)
		if !ok {
			return nil, false
		}
		if scale != 0 {
			return value * scale, true
		}
		return value, true
	default:
		return raw, true
	}
}

// encodeValue 把 JSON 解码的值按 dataType 转为 Go 原生类型,供 ua.NewVariant 构造写请求。
func encodeValue(value interface{}, dataType model.DataType) (interface{}, bool) {
	switch dataType {
	case model.DataTypeBool:
		b, ok := value.(bool)
		return b, ok
	case model.DataTypeString:
		s, ok := value.(string)
		return s, ok
	case model.DataTypeInt16:
		v, ok := toFloat64(value)
		return int16(v), ok
	case model.DataTypeUInt16:
		v, ok := toFloat64(value)
		return uint16(v), ok
	case model.DataTypeInt32:
		v, ok := toFloat64(value)
		return int32(v), ok
	case model.DataTypeUInt32:
		v, ok := toFloat64(value)
		return uint32(v), ok
	case model.DataTypeInt64:
		v, ok := toFloat64(value)
		return int64(v), ok
	case model.DataTypeFloat:
		v, ok := toFloat64(value)
		return float32(v), ok
	case model.DataTypeDouble:
		v, ok := toFloat64(value)
		return float64(v), ok
	default:
		return nil, false
	}
}

func toFloat64(raw interface{}) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	default:
		return 0, false
	}
}

func toInt64(raw interface{}) (int64, bool) {
	switch value := raw.(type) {
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		return int64(value), true
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		return int64(value), true
	case float32:
		return int64(value), true
	case float64:
		return int64(value), true
	default:
		return 0, false
	}
}

type connConfig struct {
	Endpoint     string `json:"endpoint"`
	SecurityMode string `json:"securityMode"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Timeout      string `json:"timeout"`
}

func parseConnConfig(raw json.RawMessage) (connConfig, error) {
	cfg := connConfig{SecurityMode: "none", Timeout: "5s"}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return connConfig{}, fmt.Errorf("parse opcua conn config: %w", err)
		}
	}
	if cfg.Endpoint == "" {
		return connConfig{}, errors.New("opcua endpoint is required")
	}
	if cfg.SecurityMode == "" {
		cfg.SecurityMode = "none"
	}
	if cfg.SecurityMode != "none" {
		return connConfig{}, fmt.Errorf("opcua securityMode %q not supported (only \"none\" for now)", cfg.SecurityMode)
	}
	if cfg.Timeout == "" {
		cfg.Timeout = "5s"
	}
	if _, err := time.ParseDuration(cfg.Timeout); err != nil {
		return connConfig{}, fmt.Errorf("invalid timeout %q: %w", cfg.Timeout, err)
	}
	return cfg, nil
}

const opcuaReconnectInterval = 5 * time.Second

// buildClient 建立启用了自动重连的 OPC UA client:连接断开后库自动重连,
// 状态变更经 stateCh 输出供 monitorConnState 记录离线/恢复。
func buildClient(ctx context.Context, cfg connConfig) (*opcua.Client, error) {
	timeout, _ := time.ParseDuration(cfg.Timeout)
	stateCh := make(chan opcua.ConnState, 8)
	opts := []opcua.Option{
		opcua.SecurityMode(ua.MessageSecurityModeNone),
		opcua.RequestTimeout(timeout),
		opcua.AutoReconnect(true),
		opcua.ReconnectInterval(opcuaReconnectInterval),
		opcua.StateChangedCh(stateCh),
	}
	if cfg.Username != "" {
		opts = append(opts, opcua.AuthUsername(cfg.Username, cfg.Password))
	} else {
		opts = append(opts, opcua.AuthAnonymous())
	}
	client, err := opcua.NewClient(cfg.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("create opcua client: %w", err)
	}
	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connect opcua %q: %w", cfg.Endpoint, err)
	}
	go monitorConnState(ctx, cfg.Endpoint, stateCh)
	return client, nil
}

// monitorConnState 记录连接状态变更(离线/重连/恢复),ctx 取消或连接 Closed 时退出。
func monitorConnState(ctx context.Context, endpoint string, stateCh <-chan opcua.ConnState) {
	for {
		select {
		case <-ctx.Done():
			return
		case state, ok := <-stateCh:
			if !ok {
				return
			}
			switch state {
			case opcua.Connected:
				slog.Info("opcua connected", "endpoint", endpoint)
			case opcua.Disconnected:
				slog.Warn("opcua disconnected, auto-reconnecting", "endpoint", endpoint)
			case opcua.Reconnecting:
				slog.Info("opcua reconnecting", "endpoint", endpoint)
			case opcua.Closed:
				return
			}
		}
	}
}
