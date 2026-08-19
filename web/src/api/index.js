import axios from 'axios'

// 开发环境经 vite 代理 /api -> 网关后端;生产部署时由网关(或网关前反代)提供 /api。
const http = axios.create({
  baseURL: '/api/v1',
  timeout: 10000,
})

const TOKEN_KEY = 'iotgw_token'

export function getToken() {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setToken(token) {
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

// 请求拦截:自动附带 Bearer token。
http.interceptors.request.use((config) => {
  const token = getToken()
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

// 响应拦截:解包 data;401 时清 token 并跳登录(登录接口自身除外)。
http.interceptors.response.use(
  (res) => res.data,
  (err) => {
    const status = err.response?.status
    const code = err.response?.data?.code
    if (status === 401 && !err.config?.url?.includes('/auth/login')) {
      setToken('')
      if (!window.location.pathname.startsWith('/login')) {
        window.location.assign('/login')
      }
    }
    const msg = err.response?.data?.error || err.message || '请求失败'
    const e = new Error(msg)
    e.code = code
    return Promise.reject(e)
  },
)

export const api = {
  // 鉴权
  authStatus: () => http.get('/auth/status'),
  login: (data) => http.post('/auth/login', data),
  logout: () => http.post('/auth/logout'),
  me: () => http.get('/auth/me'),
  changePassword: (data) => http.put('/auth/password', data),

  // 版本信息(公开)
  getVersion: () => http.get('/version'),

  // 三方 client
  listClients: () => http.get('/clients'),
  createClient: (data) => http.post('/clients', data),
  updateClient: (id, data) => http.put(`/clients/${id}`, data),
  deleteClient: (id) => http.delete(`/clients/${id}`),

  // 连接
  listConnections: () => http.get('/connections'),
  createConnection: (data) => http.post('/connections', data),
  updateConnection: (id, data) => http.put(`/connections/${id}`, data),
  deleteConnection: (id) => http.delete(`/connections/${id}`),
  // 批量操作(action: delete;ids 为连接 id 列表)
  batchConnections: (action, ids) => http.post('/connections/batch', { action, ids }),
  // 节点浏览(OPC UA 等支持 Browse 的驱动,供点位编辑选择节点)
  browseConnection: (id, parent) => http.post(`/connections/${id}/browse`, { parent }),

  // 北向输出
  listOutputs: () => http.get('/outputs'),
  listOutputTypes: () => http.get('/outputs/types'),
  listOutputStatus: () => http.get('/outputs/status'),
  createOutput: (data) => http.post('/outputs', data),
  updateOutput: (id, data) => http.put(`/outputs/${id}`, data),
  deleteOutput: (id) => http.delete(`/outputs/${id}`),

  // 设备
  listDevices: () => http.get('/devices'),
  createDevice: (data) => http.post('/devices', data),
  updateDevice: (id, data) => http.put(`/devices/${id}`, data),
  deleteDevice: (id) => http.delete(`/devices/${id}`),
  cloneDevice: (id, data) => http.post(`/devices/${id}/clone`, data),
  writeDevice: (id, data) => http.post(`/devices/${id}/write`, data),
  // 批量操作(action: delete | setEnabled;setEnabled 需 enabled 字段)
  batchDevices: (action, ids, enabled) => http.post('/devices/batch', { action, ids, enabled }),

  // 状态
  listStatus: () => http.get('/status'),
  getDeviceValues: (id) => http.get(`/devices/${id}/values`),
  // 边缘处理运行统计
  getProcessingStatus: () => http.get('/processing/status'),

  // 网关设置
  getGateway: () => http.get('/gateway'),
  updateGateway: (data) => http.put('/gateway', data),

  // 驱动
  listDrivers: () => http.get('/drivers'),

  // 告警规则
  listAlertRules: () => http.get('/alert-rules'),
  createAlertRule: (data) => http.post('/alert-rules', data),
  getAlertRule: (id) => http.get(`/alert-rules/${id}`),
  updateAlertRule: (id, data) => http.put(`/alert-rules/${id}`, data),
  deleteAlertRule: (id) => http.delete(`/alert-rules/${id}`),
  listAlerts: (status) => http.get('/alerts', { params: { status } }),
}

export default api
