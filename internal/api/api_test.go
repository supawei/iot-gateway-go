package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"iot-gateway-go/internal/auth"
	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
	"iot-gateway-go/internal/values"
)

func newTestAPI(t *testing.T) *API {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// 默认关闭鉴权,保持既有测试行为(与历史接口一致);鉴权相关测试单独开启。
	return New(st, status.NewRegistry(), values.NewRegistry(), nil, false, nil)
}

func doRequest(t *testing.T, handler http.Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func sampleConnection() model.Connection {
	return model.Connection{
		ID:     "conn-1",
		Name:   "modbus-tcp",
		Driver: "modbus",
		Config: json.RawMessage(`{"mode":"tcp","address":"127.0.0.1:502"}`),
	}
}

// seedConnection 预置连接,device 测试依赖 connection_id 存在。
func seedConnection(t *testing.T, handler http.Handler) {
	t.Helper()
	rec := doRequest(t, handler, "POST", "/api/v1/connections", sampleConnection())
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed connection: got %d", rec.Code)
	}
}

func sampleDevice() model.Device {
	return model.Device{
		ID:           "sensor-01",
		Name:         "温湿度",
		ConnectionID: "conn-1",
		Params:       json.RawMessage(`{"slaveId":1}`),
		IntervalMs:   1000,
		Enabled:      true,
		Points: []model.Point{
			{Name: "temperature", Address: "holding:0", DataType: model.DataTypeInt16, Scale: 0.1},
		},
	}
}

func TestCreateAndGetConnection(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	rec := doRequest(t, handler, "POST", "/api/v1/connections", sampleConnection())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want %d", rec.Code, http.StatusCreated)
	}
	rec = doRequest(t, handler, "GET", "/api/v1/connections/conn-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d want %d", rec.Code, http.StatusOK)
	}
	var got model.Connection
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Driver != "modbus" {
		t.Fatalf("unexpected connection: %+v", got)
	}
}

func TestDeleteConnectionBlockedByDevice(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	seedConnection(t, handler)
	doRequest(t, handler, "POST", "/api/v1/devices", sampleDevice())

	rec := doRequest(t, handler, "DELETE", "/api/v1/connections/conn-1", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete referenced connection: got %d want %d", rec.Code, http.StatusConflict)
	}
}

// endpointDriverMock 按配置里的 endpoint 字段返回共享命名空间的端点 key,
// 模拟支持冲突检测的驱动;两个注册名模拟"不同协议的驱动撞同一物理端点"。
type endpointDriverMock struct{}

func (*endpointDriverMock) Open(context.Context, driver.OpenRequest) (driver.Conn, error) {
	return nil, errors.New("not implemented")
}

func (*endpointDriverMock) EndpointKey(config json.RawMessage) string {
	var cfg struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil || cfg.Endpoint == "" {
		return ""
	}
	return "serial|" + cfg.Endpoint
}

// TestCreateConnectionEndpointConflict 验证两个连接(可跨驱动)指向同一物理端点被
// 409 拒绝;改指向其他端点、以及更新自身(同 ID)不受影响。
func TestCreateConnectionEndpointConflict(t *testing.T) {
	driver.Register("mock-endpoint-a", &endpointDriverMock{})
	driver.Register("mock-endpoint-b", &endpointDriverMock{})
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	first := model.Connection{ID: "conn-a", Name: "a", Driver: "mock-endpoint-a",
		Config: json.RawMessage(`{"endpoint":"/dev/ttyS0"}`)}
	if rec := doRequest(t, handler, "POST", "/api/v1/connections", first); rec.Code != http.StatusCreated {
		t.Fatalf("create first: got %d", rec.Code)
	}

	sameDriver := model.Connection{ID: "conn-b", Name: "b", Driver: "mock-endpoint-a",
		Config: json.RawMessage(`{"endpoint":"/dev/ttyS0"}`)}
	if rec := doRequest(t, handler, "POST", "/api/v1/connections", sameDriver); rec.Code != http.StatusConflict {
		t.Fatalf("create duplicate endpoint: got %d want %d", rec.Code, http.StatusConflict)
	}

	// 跨驱动撞同一串口同样拒绝:一个串口只允许一个连接,与协议无关
	crossDriver := model.Connection{ID: "conn-c", Name: "c", Driver: "mock-endpoint-b",
		Config: json.RawMessage(`{"endpoint":"/dev/ttyS0"}`)}
	if rec := doRequest(t, handler, "POST", "/api/v1/connections", crossDriver); rec.Code != http.StatusConflict {
		t.Fatalf("create cross-driver duplicate endpoint: got %d want %d", rec.Code, http.StatusConflict)
	}

	other := model.Connection{ID: "conn-b", Name: "b", Driver: "mock-endpoint-a",
		Config: json.RawMessage(`{"endpoint":"/dev/ttyS1"}`)}
	if rec := doRequest(t, handler, "POST", "/api/v1/connections", other); rec.Code != http.StatusCreated {
		t.Fatalf("create other endpoint: got %d", rec.Code)
	}

	// 更新自身(改名,端点不变)不应误判冲突
	first.Name = "renamed"
	if rec := doRequest(t, handler, "PUT", "/api/v1/connections/conn-a", first); rec.Code != http.StatusOK {
		t.Fatalf("update self: got %d want %d", rec.Code, http.StatusOK)
	}

	// 把 conn-b 改到 conn-a 的端点应被拒绝
	other.Config = json.RawMessage(`{"endpoint":"/dev/ttyS0"}`)
	if rec := doRequest(t, handler, "PUT", "/api/v1/connections/conn-b", other); rec.Code != http.StatusConflict {
		t.Fatalf("update to duplicate endpoint: got %d want %d", rec.Code, http.StatusConflict)
	}
}

// TestCreateAndGetDevice 验证创建后能用真实设备 ID 获取。
// 回归:若路由误配为固定字面量,GET /devices/sensor-01 会返回 404。
func TestCreateAndGetDevice(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)

	rec := doRequest(t, handler, "POST", "/api/v1/devices", sampleDevice())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want %d", rec.Code, http.StatusCreated)
	}

	rec = doRequest(t, handler, "GET", "/api/v1/devices/sensor-01", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d want %d (路径参数未匹配?)", rec.Code, http.StatusOK)
	}
	var got model.Device
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "sensor-01" || len(got.Points) != 1 {
		t.Fatalf("unexpected device: %+v", got)
	}
}

func TestGetDeviceNotFound(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	rec := doRequest(t, handler, "GET", "/api/v1/devices/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get nonexistent: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestListDevices(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)

	doRequest(t, handler, "POST", "/api/v1/devices", sampleDevice())
	second := sampleDevice()
	second.ID = "sensor-02"
	doRequest(t, handler, "POST", "/api/v1/devices", second)

	rec := doRequest(t, handler, "GET", "/api/v1/devices", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d want %d", rec.Code, http.StatusOK)
	}
	var devices []model.Device
	if err := json.Unmarshal(rec.Body.Bytes(), &devices); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("want 2 devices got %d", len(devices))
	}
}

// TestUpdateDeviceReplacesPoints 验证 PUT 整体替换点位。
func TestUpdateDeviceReplacesPoints(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)

	doRequest(t, handler, "POST", "/api/v1/devices", sampleDevice())

	updated := sampleDevice()
	updated.Points = []model.Point{{Name: "pressure", Address: "holding:2", DataType: model.DataTypeFloat}}
	rec := doRequest(t, handler, "PUT", "/api/v1/devices/sensor-01", updated)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: got %d want %d", rec.Code, http.StatusOK)
	}

	rec = doRequest(t, handler, "GET", "/api/v1/devices/sensor-01", nil)
	var got model.Device
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Points) != 1 || got.Points[0].Name != "pressure" {
		t.Fatalf("points not replaced: %+v", got.Points)
	}
}

func TestDeleteDevice(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)

	doRequest(t, handler, "POST", "/api/v1/devices", sampleDevice())

	rec := doRequest(t, handler, "DELETE", "/api/v1/devices/sensor-01", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d want %d", rec.Code, http.StatusNoContent)
	}

	rec = doRequest(t, handler, "GET", "/api/v1/devices/sensor-01", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

// TestAddAndDeletePoint 验证点位增删,路径含 {deviceId} 与 {name} 两段参数。
func TestAddAndDeletePoint(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)

	device := sampleDevice()
	device.Points = nil
	doRequest(t, handler, "POST", "/api/v1/devices", device)

	point := model.Point{Name: "pressure", Address: "holding:2", DataType: model.DataTypeFloat}
	rec := doRequest(t, handler, "POST", "/api/v1/devices/sensor-01/points", point)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add point: got %d want %d", rec.Code, http.StatusCreated)
	}

	rec = doRequest(t, handler, "GET", "/api/v1/devices/sensor-01", nil)
	var got model.Device
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Points) != 1 {
		t.Fatalf("point not added: %+v", got.Points)
	}

	rec = doRequest(t, handler, "DELETE", "/api/v1/devices/sensor-01/points/pressure", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete point: got %d want %d", rec.Code, http.StatusNoContent)
	}

	rec = doRequest(t, handler, "GET", "/api/v1/devices/sensor-01", nil)
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Points) != 0 {
		t.Fatalf("point not deleted: %+v", got.Points)
	}
}

// TestCloneDevice 验证复制:未提供字段继承源设备,points 整体拷贝,params 可覆盖。
func TestCloneDevice(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)
	doRequest(t, handler, "POST", "/api/v1/devices", sampleDevice())

	cloneBody := map[string]any{
		"id":     "sensor-02",
		"name":   "温湿度-2",
		"params": map[string]any{"slaveId": 2},
	}
	rec := doRequest(t, handler, "POST", "/api/v1/devices/sensor-01/clone", cloneBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone: got %d want %d", rec.Code, http.StatusCreated)
	}
	var cloned model.Device
	if err := json.Unmarshal(rec.Body.Bytes(), &cloned); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cloned.ID != "sensor-02" || cloned.Name != "温湿度-2" {
		t.Fatalf("unexpected clone identity: %+v", cloned)
	}
	if cloned.ConnectionID != "conn-1" || cloned.IntervalMs != 1000 || !cloned.Enabled {
		t.Fatalf("clone did not inherit from source: %+v", cloned)
	}
	if len(cloned.Points) != 1 || cloned.Points[0].Name != "temperature" {
		t.Fatalf("clone did not copy points: %+v", cloned.Points)
	}
	var params map[string]any
	json.Unmarshal(cloned.Params, &params)
	if params["slaveId"] != float64(2) {
		t.Fatalf("clone params not overridden: %s", cloned.Params)
	}
}

// TestCreateAutoID 验证连接/设备新增不带 id 时由后台生成(conn-/dev- 前缀),
// 两次生成互不相同;克隆不带 id 同样自动生成。
func TestCreateAutoID(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	newConn := sampleConnection()
	newConn.ID = ""
	rec := doRequest(t, handler, "POST", "/api/v1/connections", newConn)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create connection without id: got %d", rec.Code)
	}
	var createdConn model.Connection
	if err := json.Unmarshal(rec.Body.Bytes(), &createdConn); err != nil {
		t.Fatalf("unmarshal connection: %v", err)
	}
	if !strings.HasPrefix(createdConn.ID, "conn-") || len(createdConn.ID) == len("conn-") {
		t.Fatalf("connection id not auto-generated: %q", createdConn.ID)
	}

	device := sampleDevice()
	device.ID = ""
	device.ConnectionID = createdConn.ID
	rec = doRequest(t, handler, "POST", "/api/v1/devices", device)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create device without id: got %d", rec.Code)
	}
	var firstCreated model.Device
	if err := json.Unmarshal(rec.Body.Bytes(), &firstCreated); err != nil {
		t.Fatalf("unmarshal device: %v", err)
	}
	if !strings.HasPrefix(firstCreated.ID, "dev-") || len(firstCreated.ID) == len("dev-") {
		t.Fatalf("device id not auto-generated: %q", firstCreated.ID)
	}

	// 第二台不带 id 的设备应得到不同 id
	device.Name = "第二台"
	rec = doRequest(t, handler, "POST", "/api/v1/devices", device)
	var secondCreated model.Device
	json.Unmarshal(rec.Body.Bytes(), &secondCreated)
	if secondCreated.ID == firstCreated.ID {
		t.Fatalf("auto ids collide: %q", firstCreated.ID)
	}

	rec = doRequest(t, handler, "POST", "/api/v1/devices/"+firstCreated.ID+"/clone", map[string]any{"name": "克隆"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone without id: got %d", rec.Code)
	}
	var cloned model.Device
	json.Unmarshal(rec.Body.Bytes(), &cloned)
	if !strings.HasPrefix(cloned.ID, "dev-") || cloned.ID == firstCreated.ID {
		t.Fatalf("clone id not auto-generated: %q", cloned.ID)
	}
}

func TestCloneDeviceMissingSource(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	rec := doRequest(t, handler, "POST", "/api/v1/devices/nonexistent/clone",
		map[string]any{"name": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("clone missing source: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestCloneDeviceMissingName(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)
	doRequest(t, handler, "POST", "/api/v1/devices", sampleDevice())

	rec := doRequest(t, handler, "POST", "/api/v1/devices/sensor-01/clone",
		map[string]any{"id": "sensor-02"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clone missing name: got %d want %d", rec.Code, http.StatusBadRequest)
	}
}

// mockDriver 返回预设 conn,供写测试脱离真协议驱动。
type mockDriver struct{ conn driver.Conn }

func (m *mockDriver) Open(context.Context, driver.OpenRequest) (driver.Conn, error) {
	return m.conn, nil
}

// writableConn 实现 Conn + Writer,Write 恒成功。
type writableConn struct{}

func (c *writableConn) Read(context.Context, []model.Point) ([]model.DataPoint, error) {
	return nil, nil
}
func (c *writableConn) Close() error { return nil }
func (c *writableConn) Write(_ context.Context, items []model.WriteItem) ([]driver.WriteResult, error) {
	results := make([]driver.WriteResult, len(items))
	for i, item := range items {
		results[i] = driver.WriteResult{Point: item.Point.Name, Ok: true}
	}
	return results, nil
}

// readonlyConn 仅实现 Conn,不实现 Writer(类型断言失败 -> 501)。
type readonlyConn struct{}

func (c *readonlyConn) Read(context.Context, []model.Point) ([]model.DataPoint, error) {
	return nil, nil
}
func (c *readonlyConn) Close() error { return nil }

func seedWritableDevice(t *testing.T, handler http.Handler, deviceID string) {
	t.Helper()
	doRequest(t, handler, "POST", "/api/v1/connections", model.Connection{
		ID: "c1", Name: "mock", Driver: "mock-writable", Config: json.RawMessage(`{}`),
	})
	doRequest(t, handler, "POST", "/api/v1/devices", model.Device{
		ID: deviceID, Name: "dev", ConnectionID: "c1",
		Points: []model.Point{{Name: "setpoint", Address: "holding:0", DataType: model.DataTypeInt16}},
	})
}

func TestWriteDevice(t *testing.T) {
	driver.Register("mock-writable", &mockDriver{conn: &writableConn{}})
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedWritableDevice(t, handler, "d1")

	rec := doRequest(t, handler, "POST", "/api/v1/devices/d1/write",
		map[string]any{"point": "setpoint", "value": 42})
	if rec.Code != http.StatusOK {
		t.Fatalf("write: got %d want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var results []driver.WriteResult
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(results) != 1 || !results[0].Ok || results[0].Point != "setpoint" {
		t.Fatalf("write result: %+v", results)
	}
}

func TestWriteDeviceNotFound(t *testing.T) {
	driver.Register("mock-writable", &mockDriver{conn: &writableConn{}})
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	rec := doRequest(t, handler, "POST", "/api/v1/devices/nonexistent/write",
		map[string]any{"point": "setpoint", "value": 1})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("device not found: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestWriteDevicePointNotFound(t *testing.T) {
	driver.Register("mock-writable", &mockDriver{conn: &writableConn{}})
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedWritableDevice(t, handler, "d1")

	rec := doRequest(t, handler, "POST", "/api/v1/devices/d1/write",
		map[string]any{"point": "missing", "value": 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("point not found: got %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWriteDeviceDriverNotWritable(t *testing.T) {
	driver.Register("mock-readonly", &mockDriver{conn: &readonlyConn{}})
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	doRequest(t, handler, "POST", "/api/v1/connections", model.Connection{
		ID: "c1", Name: "mock", Driver: "mock-readonly", Config: json.RawMessage(`{}`),
	})
	doRequest(t, handler, "POST", "/api/v1/devices", model.Device{
		ID: "d1", Name: "dev", ConnectionID: "c1",
		Points: []model.Point{{Name: "setpoint", Address: "holding:0", DataType: model.DataTypeInt16}},
	})

	rec := doRequest(t, handler, "POST", "/api/v1/devices/d1/write",
		map[string]any{"point": "setpoint", "value": 1})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("readonly driver: got %d want %d", rec.Code, http.StatusNotImplemented)
	}
}

// TestStatusEndpoints 验证设备运行时状态查询(列表 + 单设备)。
func TestStatusEndpoints(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	reg := status.NewRegistry()
	reg.SetOnline("d1", time.Now())
	reg.SetOffline("d2", "connection refused")

	handler := New(st, reg, values.NewRegistry(), nil, false, nil).Routes()

	rec := doRequest(t, handler, "GET", "/api/v1/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status: got %d", rec.Code)
	}
	var list []status.DeviceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 status got %d", len(list))
	}

	rec = doRequest(t, handler, "GET", "/api/v1/devices/d1/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status: got %d", rec.Code)
	}
	var got status.DeviceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DeviceID != "d1" || !got.Online {
		t.Fatalf("status: %+v", got)
	}
}

func TestGetDeviceStatusNotFound(t *testing.T) {
	apiInstance := newTestAPI(t)
	rec := doRequest(t, apiInstance.Routes(), "GET", "/api/v1/devices/nonexistent/status", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status not found: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

// TestGetDeviceValues 验证实时值查询:返回各点位最新值,设备从未上报时返回空列表。
func TestGetDeviceValues(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	reg := values.NewRegistry()
	reg.Update(model.DataPoint{
		DeviceID: "d1", Point: "temperature", Value: 25.5,
		Quality: model.QualityGood, Timestamp: time.Now(),
	})
	reg.Update(model.DataPoint{
		DeviceID: "d1", Point: "humidity", Value: 60.0,
		Quality: model.QualityGood, Timestamp: time.Now(),
	})

	handler := New(st, status.NewRegistry(), reg, nil, false, nil).Routes()

	rec := doRequest(t, handler, "GET", "/api/v1/devices/d1/values", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get values: got %d", rec.Code)
	}
	var got values.DeviceValues
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DeviceID != "d1" || len(got.Points) != 2 {
		t.Fatalf("values: %+v", got)
	}

	// 从未上报的设备返回空列表(而非 404 或 null)
	rec = doRequest(t, handler, "GET", "/api/v1/devices/empty/values", nil)
	var empty values.DeviceValues
	json.Unmarshal(rec.Body.Bytes(), &empty)
	if empty.DeviceID != "empty" || len(empty.Points) != 0 {
		t.Fatalf("empty values: %+v", empty)
	}
}

// schemaDriverMock 实现 Driver + SchemaProvider,供驱动列表端点测试。
type schemaDriverMock struct{ conn driver.Conn }

func (m *schemaDriverMock) Open(context.Context, driver.OpenRequest) (driver.Conn, error) {
	return m.conn, nil
}
func (m *schemaDriverMock) ConfigSchema() []driver.Field {
	return []driver.Field{{Name: "endpoint", Label: "端点", Type: driver.FieldString}}
}
func (m *schemaDriverMock) ParamSchema() []driver.Field { return nil }

func TestListDriversEndpoint(t *testing.T) {
	driver.Register("api-schema-driver", &schemaDriverMock{conn: &readonlyConn{}})
	apiInstance := newTestAPI(t)

	rec := doRequest(t, apiInstance.Routes(), "GET", "/api/v1/drivers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list drivers: got %d", rec.Code)
	}
	var infos []driver.DriverInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &infos); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, info := range infos {
		if info.Name == "api-schema-driver" {
			if len(info.Config) != 1 || info.Config[0].Name != "endpoint" {
				t.Fatalf("config schema: %+v", info.Config)
			}
			return
		}
	}
	t.Fatal("api-schema-driver not in response")
}

// ---- 鉴权与授权 ----

// newAuthAPI 构造开启鉴权的测试 API,并预置管理员。
func newAuthAPI(t *testing.T) (*API, *auth.Manager) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	authz := auth.NewManager(st, time.Hour)
	if _, err := authz.BootstrapAdmin(); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	return New(st, status.NewRegistry(), values.NewRegistry(), authz, true, nil), authz
}

// loginAs 登录并返回 Bearer token;默认用预置管理员账号。
func loginAs(t *testing.T, handler http.Handler, user, pass string) (string, bool) {
	t.Helper()
	rec := doRequest(t, handler, "POST", "/api/v1/auth/login", map[string]string{"username": user, "password": pass})
	if rec.Code != http.StatusOK {
		return "", false
	}
	var resp struct {
		Token              string `json:"token"`
		MustChangePassword bool   `json:"mustChangePassword"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.Token, resp.MustChangePassword
}

func doRequestAuth(t *testing.T, handler http.Handler, method, path, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestLoginAndProtectedEndpoint(t *testing.T) {
	apiInstance, _ := newAuthAPI(t)
	handler := apiInstance.Routes()

	// 未带 token 访问业务接口 → 401
	rec := doRequest(t, handler, "GET", "/api/v1/status", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d want 401", rec.Code)
	}

	// 登录拿 token(首次须改密)
	token, mustChange := loginAs(t, handler, auth.DefaultAdminUser, auth.DefaultAdminPassword)
	if token == "" || !mustChange {
		t.Fatalf("login: token=%q mustChange=%v", token, mustChange)
	}

	// 首次登录未改密访问业务接口 → 403(password_change_required)
	rec = doRequestAuth(t, handler, "GET", "/api/v1/status", token, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("must change password: got %d want 403", rec.Code)
	}

	// 改密后可正常访问
	rec = doRequestAuth(t, handler, "PUT", "/api/v1/auth/password", token,
		map[string]string{"oldPassword": auth.DefaultAdminPassword, "newPassword": "newpass123"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change password: got %d", rec.Code)
	}
	rec = doRequestAuth(t, handler, "GET", "/api/v1/status", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("after change password: got %d want 200", rec.Code)
	}
}

func TestScopeAuthorization(t *testing.T) {
	apiInstance, authz := newAuthAPI(t)
	handler := apiInstance.Routes()

	// 创建一个只读三方 client
	c, key, err := authz.CreateClient("mes-ro", "只读", []string{"devices:read", "status:read", "values:read"})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if c.ID == "" || key == "" {
		t.Fatal("create client result empty")
	}

	// 允许读
	rec := doRequestAuth(t, handler, "GET", "/api/v1/status", key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("client read status: got %d want 200", rec.Code)
	}
	rec = doRequestAuth(t, handler, "GET", "/api/v1/devices", key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("client read devices: got %d want 200", rec.Code)
	}
	// 拒绝写
	rec = doRequestAuth(t, handler, "POST", "/api/v1/devices", key, map[string]any{"id": "d1"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("client write devices: got %d want 403", rec.Code)
	}
}

func TestClientsCRUDRequiresAdmin(t *testing.T) {
	apiInstance, authz := newAuthAPI(t)
	handler := apiInstance.Routes()

	// 未登录 → 401
	rec := doRequest(t, handler, "GET", "/api/v1/clients", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("list clients no token: got %d want 401", rec.Code)
	}

	// admin 登录改密后 → 可管理
	token, _ := loginAs(t, handler, auth.DefaultAdminUser, auth.DefaultAdminPassword)
	doRequestAuth(t, handler, "PUT", "/api/v1/auth/password", token,
		map[string]string{"oldPassword": auth.DefaultAdminPassword, "newPassword": "newpass123"})

	rec = doRequestAuth(t, handler, "POST", "/api/v1/clients", token,
		map[string]any{"id": "mes-ro", "name": "MES", "scopes": []string{"devices:read"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		APIKey string `json:"apiKey"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID != "mes-ro" || created.APIKey == "" {
		t.Fatalf("create client response: %+v", created)
	}

	rec = doRequestAuth(t, handler, "GET", "/api/v1/clients", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list clients: got %d", rec.Code)
	}

	rec = doRequestAuth(t, handler, "DELETE", "/api/v1/clients/mes-ro", token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete client: got %d", rec.Code)
	}

	// 三方 client 无权管理 clients(即便有 devices:read)
	_, key, _ := authz.CreateClient("other", "other", []string{"devices:read"})
	rec = doRequestAuth(t, handler, "GET", "/api/v1/clients", key, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("client list clients: got %d want 403", rec.Code)
	}
}

func TestAuthStatusEndpoint(t *testing.T) {
	enabled, _ := newAuthAPI(t)
	rec := doRequest(t, enabled.Routes(), "GET", "/api/v1/auth/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth status: got %d", rec.Code)
	}
	var body map[string]bool
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["enabled"] != true {
		t.Fatalf("auth status enabled should be true: %v", body)
	}
}

func TestAuthDisabledBypasses(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	// 鉴权关闭:业务接口无需 token(逃生舱,兼容旧部署)
	handler := New(st, status.NewRegistry(), values.NewRegistry(), nil, false, nil).Routes()
	rec := doRequest(t, handler, "GET", "/api/v1/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth disabled: got %d want 200", rec.Code)
	}
}

// ---- 北向输出 ----

func sampleOutput() model.Output {
	return model.Output{
		ID:      "mqtt-1",
		Name:    "MQTT 主站",
		Type:    "mqtt",
		Config:  json.RawMessage(`{"broker":"tcp://127.0.0.1:1883","qos":1}`),
		Enabled: true,
	}
}

// TestOutputCRUD 验证输出的增删改查(鉴权关闭 + 未接线输出管理器)。
func TestOutputCRUD(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	rec := doRequest(t, handler, "POST", "/api/v1/outputs", sampleOutput())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create output: got %d want %d, body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	rec = doRequest(t, handler, "GET", "/api/v1/outputs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list outputs: got %d", rec.Code)
	}
	var list []model.Output
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 || list[0].ID != "mqtt-1" {
		t.Fatalf("unexpected outputs: %+v", list)
	}

	rec = doRequest(t, handler, "GET", "/api/v1/outputs/mqtt-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get output: got %d", rec.Code)
	}

	updated := sampleOutput()
	updated.Name = "MQTT 备站"
	rec = doRequest(t, handler, "PUT", "/api/v1/outputs/mqtt-1", updated)
	if rec.Code != http.StatusOK {
		t.Fatalf("update output: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, handler, "DELETE", "/api/v1/outputs/mqtt-1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete output: got %d", rec.Code)
	}

	rec = doRequest(t, handler, "GET", "/api/v1/outputs/mqtt-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: got %d want 404", rec.Code)
	}
}

// TestCreateOutputMissingID 验证创建缺 id 返回 400。
func TestCreateOutputMissingID(t *testing.T) {
	apiInstance := newTestAPI(t)
	o := sampleOutput()
	o.ID = ""
	rec := doRequest(t, apiInstance.Routes(), "POST", "/api/v1/outputs", o)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create missing id: got %d want 400", rec.Code)
	}
}

// TestOutputTypesEndpoint 验证 /outputs/types 返回注册的输出类型与 schema。
func TestOutputTypesEndpoint(t *testing.T) {
	output.Register(output.Descriptor{
		Type:   "test-out",
		Label:  "测试输出",
		Schema: []output.Field{{Name: "broker", Label: "地址", Type: output.FieldString, Required: true}},
	}, func(output.BuildContext, json.RawMessage) (output.Output, error) {
		return nil, nil
	})

	apiInstance := newTestAPI(t)
	rec := doRequest(t, apiInstance.Routes(), "GET", "/api/v1/outputs/types", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list output types: got %d", rec.Code)
	}
	var types []output.Descriptor
	if err := json.Unmarshal(rec.Body.Bytes(), &types); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, info := range types {
		if info.Type == "test-out" {
			if len(info.Schema) != 1 || info.Schema[0].Name != "broker" {
				t.Fatalf("schema: %+v", info.Schema)
			}
			return
		}
	}
	t.Fatal("test-out not in response")
}

// TestOutputsRequireScope 验证输出接口受 scope 保护。
func TestOutputsRequireScope(t *testing.T) {
	apiInstance, authz := newAuthAPI(t)
	handler := apiInstance.Routes()

	// 未登录 → 401
	rec := doRequest(t, handler, "GET", "/api/v1/outputs", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("list outputs no token: got %d want 401", rec.Code)
	}

	// 只读三方 client:可读不可写
	_, key, _ := authz.CreateClient("ro", "只读", []string{"outputs:read"})
	rec = doRequestAuth(t, handler, "GET", "/api/v1/outputs", key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("client read outputs: got %d want 200", rec.Code)
	}
	rec = doRequestAuth(t, handler, "POST", "/api/v1/outputs", key, sampleOutput())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("client write outputs: got %d want 403", rec.Code)
	}
}

// TestGatewayEndpoint 验证网关 ID 的读取与修改(默认预置,可改)。
func TestGatewayEndpoint(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	rec := doRequest(t, handler, "GET", "/api/v1/gateway", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get gateway: got %d want 200", rec.Code)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.ID != store.DefaultGatewayID {
		t.Fatalf("default gateway id = %q, want %q", body.ID, store.DefaultGatewayID)
	}

	rec = doRequest(t, handler, "PUT", "/api/v1/gateway", map[string]any{"id": "gw-02"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update gateway: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, handler, "GET", "/api/v1/gateway", nil)
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.ID != "gw-02" {
		t.Fatalf("gateway id after update = %q, want gw-02", body.ID)
	}

	// 空 ID → 400
	rec = doRequest(t, handler, "PUT", "/api/v1/gateway", map[string]any{"id": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update empty id: got %d want 400", rec.Code)
	}
}

// TestGatewayRequiresScope 验证网关设置接口受 scope 保护。
func TestGatewayRequiresScope(t *testing.T) {
	apiInstance, authz := newAuthAPI(t)
	handler := apiInstance.Routes()

	// 未登录 → 401
	rec := doRequest(t, handler, "GET", "/api/v1/gateway", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("get gateway no token: got %d want 401", rec.Code)
	}

	// 无 gateway scope 的三方 client → 403
	_, key, _ := authz.CreateClient("ro", "只读", []string{"devices:read"})
	rec = doRequestAuth(t, handler, "GET", "/api/v1/gateway", key, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("client get gateway: got %d want 403", rec.Code)
	}
}

// TestGatewayWriteTriggersReload 验证修改网关 ID 后触发热重载(MQTT topic 依赖网关 ID)。
func TestGatewayWriteTriggersReload(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var mu sync.Mutex
	var reloads int
	mgr := output.NewManager(func() ([]output.Output, error) {
		mu.Lock()
		reloads++
		mu.Unlock()
		return nil, nil
	})

	handler := New(st, status.NewRegistry(), values.NewRegistry(), nil, false, mgr).Routes()
	rec := doRequest(t, handler, "PUT", "/api/v1/gateway", map[string]any{"id": "gw-02"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update gateway: got %d, body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	got := reloads
	mu.Unlock()
	if got != 1 {
		t.Fatalf("want 1 reload got %d", got)
	}
}

// TestOutputWriteTriggersReload 验证写/删输出后触发热重载。
func TestOutputWriteTriggersReload(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var mu sync.Mutex
	var reloads int
	mgr := output.NewManager(func() ([]output.Output, error) {
		mu.Lock()
		reloads++
		mu.Unlock()
		return nil, nil
	})

	handler := New(st, status.NewRegistry(), values.NewRegistry(), nil, false, mgr).Routes()

	if rec := doRequest(t, handler, "POST", "/api/v1/outputs", sampleOutput()); rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, handler, "PUT", "/api/v1/outputs/mqtt-1", sampleOutput()); rec.Code != http.StatusOK {
		t.Fatalf("update: got %d", rec.Code)
	}
	if rec := doRequest(t, handler, "DELETE", "/api/v1/outputs/mqtt-1", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d", rec.Code)
	}

	mu.Lock()
	got := reloads
	mu.Unlock()
	if got != 3 {
		t.Fatalf("want 3 reloads got %d", got)
	}
}
