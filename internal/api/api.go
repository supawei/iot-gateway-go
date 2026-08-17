package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"iot-gateway-go/internal/auth"
	"iot-gateway-go/internal/core"
	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
	"iot-gateway-go/internal/values"
)

// API 提供 REST 配置接口,操作 store 并通过 OnChange 触发 scheduler 热加载;
// 同时提供设备运行时状态与实时值查询。所有接口经 auth 中间件做鉴权与 scope 授权。
type API struct {
	store       *store.Store
	status      *status.Registry
	values      *values.Registry
	auth        *auth.Manager
	authEnabled bool
}

func New(st *store.Store, statusReg *status.Registry, valuesReg *values.Registry, authz *auth.Manager, authEnabled bool) *API {
	return &API{store: st, status: statusReg, values: valuesReg, auth: authz, authEnabled: authEnabled}
}

// Routes 返回挂载好路由的 ServeMux,由 main 直接用作 http.Server Handler。
// 每个业务接口经 require(scope) 声明所需权限;认证相关接口单独处理。
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	// 认证(登录匿名;登出/查自己/改密仅需已认证,不受改密门禁限制)
	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.HandleFunc("POST /api/v1/auth/logout", a.requireAuth(a.logout))
	mux.HandleFunc("GET /api/v1/auth/me", a.requireAuth(a.me))
	mux.HandleFunc("PUT /api/v1/auth/password", a.requireAuth(a.changePassword))
	// 三方 client 管理(仅 admin 持有 * 可访问)
	mux.HandleFunc("GET /api/v1/clients", a.require(auth.ScopeClientsRead, a.listClients))
	mux.HandleFunc("POST /api/v1/clients", a.require(auth.ScopeClientsWrite, a.createClient))
	mux.HandleFunc("PUT /api/v1/clients/{clientId}", a.require(auth.ScopeClientsWrite, a.updateClient))
	mux.HandleFunc("DELETE /api/v1/clients/{clientId}", a.require(auth.ScopeClientsWrite, a.deleteClient))
	// 业务接口
	mux.HandleFunc("POST /api/v1/connections", a.require(auth.ScopeConnectionsWrite, a.createConnection))
	mux.HandleFunc("GET /api/v1/connections", a.require(auth.ScopeConnectionsRead, a.listConnections))
	mux.HandleFunc("GET /api/v1/connections/{connectionId}", a.require(auth.ScopeConnectionsRead, a.getConnection))
	mux.HandleFunc("PUT /api/v1/connections/{connectionId}", a.require(auth.ScopeConnectionsWrite, a.putConnection))
	mux.HandleFunc("DELETE /api/v1/connections/{connectionId}", a.require(auth.ScopeConnectionsWrite, a.deleteConnection))
	mux.HandleFunc("POST /api/v1/devices", a.require(auth.ScopeDevicesWrite, a.createDevice))
	mux.HandleFunc("GET /api/v1/devices", a.require(auth.ScopeDevicesRead, a.listDevices))
	mux.HandleFunc("GET /api/v1/devices/{deviceId}", a.require(auth.ScopeDevicesRead, a.getDevice))
	mux.HandleFunc("PUT /api/v1/devices/{deviceId}", a.require(auth.ScopeDevicesWrite, a.putDevice))
	mux.HandleFunc("DELETE /api/v1/devices/{deviceId}", a.require(auth.ScopeDevicesWrite, a.deleteDevice))
	mux.HandleFunc("POST /api/v1/devices/{deviceId}/clone", a.require(auth.ScopeDevicesWrite, a.cloneDevice))
	mux.HandleFunc("POST /api/v1/devices/{deviceId}/write", a.require(auth.ScopeDevicesCommand, a.writeDevice))
	mux.HandleFunc("POST /api/v1/devices/{deviceId}/points", a.require(auth.ScopeDevicesWrite, a.addPoint))
	mux.HandleFunc("DELETE /api/v1/devices/{deviceId}/points/{name}", a.require(auth.ScopeDevicesWrite, a.deletePoint))
	mux.HandleFunc("GET /api/v1/status", a.require(auth.ScopeStatusRead, a.listStatus))
	mux.HandleFunc("GET /api/v1/devices/{deviceId}/status", a.require(auth.ScopeStatusRead, a.getDeviceStatus))
	mux.HandleFunc("GET /api/v1/devices/{deviceId}/values", a.require(auth.ScopeValuesRead, a.getDeviceValues))
	mux.HandleFunc("GET /api/v1/drivers", a.require(auth.ScopeDriversRead, a.listDrivers))
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

// writeDevice 下发单点写:复用 core.WritePoint(查设备点位 → 打开连接 → Writer.Write)。
// 写是即时操作,不走采集循环;连接复用驱动 ConnectionID 池(写完 release)。
func (a *API) writeDevice(w http.ResponseWriter, r *http.Request) {
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Point == "" {
		writeError(w, http.StatusBadRequest, errors.New("point is required"))
		return
	}
	results, err := core.WritePoint(r.Context(), a.store, r.PathValue("deviceId"), req.Point, req.Value)
	if err != nil {
		switch {
		case errors.Is(err, core.ErrDeviceNotFound):
			writeError(w, http.StatusNotFound, err)
		case errors.Is(err, core.ErrPointNotFound):
			writeError(w, http.StatusBadRequest, err)
		case errors.Is(err, core.ErrNotWritable):
			writeError(w, http.StatusNotImplemented, err)
		default:
			writeError(w, http.StatusBadGateway, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, results)
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

// getDeviceValues 返回设备各点位的最新采集值(内存态快照);设备从未上报则返回空列表。
func (a *API) getDeviceValues(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.values.Get(r.PathValue("deviceId")))
}

// listDrivers 返回已注册驱动及其配置 schema,供前端动态渲染表单。
func (a *API) listDrivers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, driver.List())
}

// ---- 鉴权中间件 ----

// require 返回要求"已认证 + 持有 scope + 已改密(若须)"的包装 handler。
// 鉴权关闭时直接放行,保持逃生舱行为与旧版一致。
func (a *API) require(scope auth.Scope, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authEnabled {
			h(w, r)
			return
		}
		p, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		if p.MustChangePassword {
			writeErrorCode(w, http.StatusForbidden, "password_change_required", auth.ErrPasswordChangeRequired.Error())
			return
		}
		if !p.HasScope(scope) {
			writeErrorCode(w, http.StatusForbidden, "forbidden", auth.ErrAuthz.Error())
			return
		}
		h(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}
}

// requireAuth 返回仅要求"已认证"的包装 handler(登出/查自己/改密),
// 不受改密门禁限制(否则首次登录无法改密)。
func (a *API) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authEnabled {
			h(w, r)
			return
		}
		p, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		h(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}
}

// authenticate 从 Bearer 头解析主体;失败写 401 并返回 false。
func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	token := auth.BearerFromHeader(r.Header.Get("Authorization"))
	p, ok := a.auth.Authenticate(token)
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthenticated", auth.ErrAuthn.Error())
		return auth.Principal{}, false
	}
	return p, true
}

// ---- 认证接口 ----

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token              string   `json:"token"`
	ID                 string   `json:"id"`
	Scopes             []string `json:"scopes"`
	MustChangePassword bool     `json:"mustChangePassword"`
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled {
		writeError(w, http.StatusServiceUnavailable, errors.New("auth is disabled"))
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token, p, err := a.auth.Login(req.Username, req.Password)
	if err != nil {
		writeErrorCode(w, http.StatusUnauthorized, "unauthenticated", "invalid username or password")
		return
	}
	scopes := make([]string, 0, len(p.Scopes))
	for _, s := range p.Scopes {
		scopes = append(scopes, string(s))
	}
	writeJSON(w, http.StatusOK, loginResponse{
		Token:              token,
		ID:                 p.ID,
		Scopes:             scopes,
		MustChangePassword: p.MustChangePassword,
	})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if token := auth.BearerFromHeader(r.Header.Get("Authorization")); token != "" {
		a.auth.Logout(token)
	}
	w.WriteHeader(http.StatusNoContent)
}

// meResponse 是当前主体的身份信息。
type meResponse struct {
	Kind               string   `json:"kind"`
	ID                 string   `json:"id"`
	Scopes             []string `json:"scopes"`
	MustChangePassword bool     `json:"mustChangePassword"`
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		// 鉴权关闭时无主体,返回匿名身份。
		writeJSON(w, http.StatusOK, meResponse{Kind: "anonymous"})
		return
	}
	scopes := make([]string, 0, len(p.Scopes))
	for _, s := range p.Scopes {
		scopes = append(scopes, string(s))
	}
	writeJSON(w, http.StatusOK, meResponse{
		Kind:               string(p.Kind),
		ID:                 p.ID,
		Scopes:             scopes,
		MustChangePassword: p.MustChangePassword,
	})
}

type changePasswordRequest struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthenticated", auth.ErrAuthn.Error())
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, errors.New("new password is required"))
		return
	}
	if err := a.auth.ChangePassword(p.ID, req.OldPassword, req.NewPassword); err != nil {
		if errors.Is(err, auth.ErrAuthn) {
			writeErrorCode(w, http.StatusUnauthorized, "unauthenticated", "invalid old password")
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 三方 client 管理 ----

func (a *API) listClients(w http.ResponseWriter, r *http.Request) {
	clients, err := a.store.ListClients()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, clients)
}

type createClientRequest struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// createClientResponse 额外带一次性明文 API Key(仅此一次返回)。
type createClientResponse struct {
	model.Client
	APIKey string `json:"apiKey"`
}

func (a *API) createClient(w http.ResponseWriter, r *http.Request) {
	var req createClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("client id is required"))
		return
	}
	c, key, err := a.auth.CreateClient(req.ID, req.Name, req.Scopes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, createClientResponse{Client: c, APIKey: key})
}

type updateClientRequest struct {
	Scopes  []string `json:"scopes"`
	Enabled *bool    `json:"enabled"`
}

func (a *API) updateClient(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.GetClient(r.PathValue("clientId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var req updateClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Scopes != nil {
		c.Scopes = req.Scopes
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if err := a.store.SaveClient(c); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *API) deleteClient(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteClient(r.PathValue("clientId")); err != nil {
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

// writeErrorCode 返回带机器可读 code 的错误(供前端按 code 分支,如跳转改密页)。
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
