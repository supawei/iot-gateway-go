package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

// API 提供 REST 配置接口,操作 store 并通过 OnChange 触发 scheduler 热加载。
type API struct {
	store *store.Store
}

func New(st *store.Store) *API {
	return &API{store: st}
}

// Routes 返回挂载好路由的 ServeMux,由 main 直接用作 http.Server Handler。
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/devices", a.createDevice)
	mux.HandleFunc("GET /api/v1/devices", a.listDevices)
	mux.HandleFunc("GET /api/v1/devices/{deviceId}", a.getDevice)
	mux.HandleFunc("PUT /api/v1/devices/{deviceId}", a.putDevice)
	mux.HandleFunc("DELETE /api/v1/devices/{deviceId}", a.deleteDevice)
	mux.HandleFunc("POST /api/v1/devices/{deviceId}/points", a.addPoint)
	mux.HandleFunc("DELETE /api/v1/devices/{deviceId}/points/{name}", a.deletePoint)
	return mux
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
