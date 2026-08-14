import axios from 'axios'

// 开发环境经 vite 代理 /api -> 网关后端;生产部署时由网关(或网关前反代)提供 /api。
const http = axios.create({
  baseURL: '/api/v1',
  timeout: 10000,
})

http.interceptors.response.use(
  (res) => res.data,
  (err) => {
    const msg = err.response?.data?.error || err.message || '请求失败'
    return Promise.reject(new Error(msg))
  },
)

export const api = {
  // 连接
  listConnections: () => http.get('/connections'),
  createConnection: (data) => http.post('/connections', data),
  updateConnection: (id, data) => http.put(`/connections/${id}`, data),
  deleteConnection: (id) => http.delete(`/connections/${id}`),

  // 设备
  listDevices: () => http.get('/devices'),
  createDevice: (data) => http.post('/devices', data),
  updateDevice: (id, data) => http.put(`/devices/${id}`, data),
  deleteDevice: (id) => http.delete(`/devices/${id}`),
  cloneDevice: (id, data) => http.post(`/devices/${id}/clone`, data),
  writeDevice: (id, data) => http.post(`/devices/${id}/write`, data),

  // 状态
  listStatus: () => http.get('/status'),

  // 驱动
  listDrivers: () => http.get('/drivers'),
}

export default api
