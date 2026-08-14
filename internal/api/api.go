package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
)

// API 提供 REST 配置接口,操作 store 并通过 OnChange 触发 scheduler 热加载;
// 同时提供设备运行时状态查询。
type API struct {
	store  *store.Store
	status *status.Registry
}

func New(st *store.Store, statusReg *status.Registry) *API {
	return &API{store: st, status: statusReg}
}

// Routes 返回挂载好路由的 ServeMux,由 main 直接用作 http.Server Handler。
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/connections", a.createConnection)
	mux.HandleFunc("GET /api/v1/connections", a.listConnections)
	mux.HandleFunc("GET /api/v1/connections/{connectionId}", a.getConnection)
	mux.HandleFunc("PUT /api/v1/connections/{connectionId}", a.putConnection)
	mux.HandleFunc("DELETE /api/v1/connections/{connectionId}", a.deleteConnection)
	mux.HandleFunc("POST /api/v1/devices", a.createDevice)
	mux.HandleFunc("GET /api/v1/devices", a.listDevices)
	mux.HandleFunc("GET /api/v1/devices/{deviceId}", a.getDevice)
	mux.HandleFunc("PUT /api/v1/devices/{deviceId}", a.putDevice)
	mux.HandleFunc("DELETE /api/v1/devices/{deviceId}", a.deleteDevice)
	mux.HandleFunc("POST /api/v1/devices/{deviceId}/clone", a.cloneDevice)
	mux.HandleFunc("POST /api/v1/devices/{deviceId}/write", a.writeDevice)
	mux.HandleFunc("POST /api/v1/devices/{deviceId}/points", a.addPoint)
	mux.HandleFunc("DELETE /api/v1/devices/{deviceId}/points/{name}", a.deletePoint)
	mux.HandleFunc("GET /api/v1/status", a.listStatus)
	mux.HandleFunc("GET /api/v1/devices/{deviceId}/status", a.getDeviceStatus)
	mux.HandleFunc("GET /api/v1/drivers", a.listDrivers)
	return mux
}

func (a *API) createConnection(w http.ResponseWriter, r *http.Request) {
	conn, ok := decodeConnection(w, r)
	if !ok {
		return
	}
	if conn.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("connection id is required"))
		return
	}
	if err := a.store.SaveConnection(conn); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, conn)
}

func (a *API) listConnections(w http.ResponseWriter, r *http.Request) {
	conns, err := a.store.ListConnections()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, conns)
}

func (a *API) getConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := a.store.GetConnection(r.PathValue("connectionId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

func (a *API) putConnection(w http.ResponseWriter, r *http.Request) {
	conn, ok := decodeConnection(w, r)
	if !ok {
		return
	}
	conn.ID = r.PathValue("connectionId")
	if err := a.store.SaveConnection(conn); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

func (a *API) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteConnection(r.PathValue("connectionId")); err != nil {
		if errors.Is(err, store.ErrConnectionInUse) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeConnection(w http.ResponseWriter, r *http.Request) (model.Connection, bool) {
	var conn model.Connection
	if err := json.NewDecoder(r.Body).Decode(&conn); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return model.Connection{}, false
	}
	return conn, true
}

func (a *API) createDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := decodeDevice(w, r)
	if !ok {
		return
	}
	if device.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("device id is required"))
		return
	}
	if err := a.store.SaveDevice(device); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, device)
}

// cloneRequest 描述复制设备时的覆盖字段;未提供的字段从源设备继承,points 始终整体拷贝。
type cloneRequest struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	ConnectionID string          `json:"connectionId"`
	Params       json.RawMessage `json:"params"`
	IntervalMs   int             `json:"intervalMs"`
	Enabled      *bool           `json:"enabled"`
}

func (a *API) cloneDevice(w http.ResponseWriter, r *http.Request) {
	source, err := a.store.GetDevice(r.PathValue("deviceId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var req cloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("clone requires id and name"))
		return
	}
	cloned := model.Device{
		ID:           req.ID,
		Name:         req.Name,
		ConnectionID: source.ConnectionID,
		Params:       source.Params,
		Points:       source.Points,
		IntervalMs:   source.IntervalMs,
		Enabled:      source.Enabled,
	}
	if req.ConnectionID != "" {
		cloned.ConnectionID = req.ConnectionID
	}
	if len(req.Params) > 0 {
		cloned.Params = req.Params
	}
	if req.IntervalMs > 0 {
		cloned.IntervalMs = req.IntervalMs
	}
	if req.Enabled != nil {
		cloned.Enabled = *req.Enabled
	}
	if err := a.store.SaveDevice(cloned); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, cloned)
}

// writeRequest 是下发写操作的请求体:点位名 + 工程值。
type writeRequest struct {
	Point string      `json:"point"`
	Value interface{} `json:"value"`
}

// writeDevice 下发单点写:查设备点位 -> 复用驱动连接 -> 类型断言 Writer -> Write。
// 写是即时操作,不走采集循环;连接复用驱动 ConnectionID 池(写完 release)。
func (a *API) writeDevice(w http.ResponseWriter, r *http.Request) {
	device, err := a.store.GetDevice(r.PathValue("deviceId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Point == "" {
		writeError(w, http.StatusBadRequest, errors.New("point is required"))
		return
	}
	point, ok := findPoint(device.Points, req.Point)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("point %q not found on device %q", req.Point, device.ID))
		return
	}
	connection, err := a.store.GetConnection(device.ConnectionID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	drv, err := driver.Get(connection.Driver)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conn, err := drv.Open(r.Context(), driver.OpenRequest{
		DeviceID:     device.ID,
		ConnectionID: device.ConnectionID,
		ConnConfig:   connection.Config,
		DeviceParams: device.Params,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer conn.Close()
	writer, ok := conn.(driver.Writer)
	if !ok {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("driver %q does not support write", connection.Driver))
		return
	}
	results, err := writer.Write(r.Context(), []model.WriteItem{{Point: point, Value: req.Value}})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func findPoint(points []model.Point, name string) (model.Point, bool) {
	for _, point := range points {
		if point.Name == name {
			return point, true
		}
	}
	return model.Point{}, false
}

func (a *API) listDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := a.store.ListDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

func (a *API) getDevice(w http.ResponseWriter, r *http.Request) {
	device, err := a.store.GetDevice(r.PathValue("deviceId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func (a *API) putDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := decodeDevice(w, r)
	if !ok {
		return
	}
	device.ID = r.PathValue("deviceId")
	if err := a.store.SaveDevice(device); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func (a *API) deleteDevice(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteDevice(r.PathValue("deviceId")); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) addPoint(w http.ResponseWriter, r *http.Request) {
	var point model.Point
	if err := json.NewDecoder(r.Body).Decode(&point); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.store.AddPoint(r.PathValue("deviceId"), point); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, point)
}

func (a *API) deletePoint(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeletePoint(r.PathValue("deviceId"), r.PathValue("name")); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listStatus 返回全部设备的运行时状态(在线/离线、最近采集、最近错误)。
func (a *API) listStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.status.List())
}

// getDeviceStatus 返回单台设备的运行时状态;设备从未被上报过则 404。
func (a *API) getDeviceStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := a.status.Get(r.PathValue("deviceId"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("device status not found"))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// listDrivers 返回已注册驱动及其配置 schema,供前端动态渲染表单。
func (a *API) listDrivers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, driver.List())
}

func decodeDevice(w http.ResponseWriter, r *http.Request) (model.Device, bool) {
	var device model.Device
	if err := json.NewDecoder(r.Body).Decode(&device); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return model.Device{}, false
	}
	return device, true
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
