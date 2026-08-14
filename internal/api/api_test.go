package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

func newTestAPI(t *testing.T) *API {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st)
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

func TestCreateDeviceMissingID(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()
	seedConnection(t, handler)

	device := sampleDevice()
	device.ID = ""
	rec := doRequest(t, handler, "POST", "/api/v1/devices", device)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create missing id: got %d want %d", rec.Code, http.StatusBadRequest)
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
