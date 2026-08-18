import { reactive } from 'vue'
import { api, setToken } from '../api'
import { encryptField } from '../utils/crypto'

// 前端鉴权状态:登录态、当前主体、是否须改密、鉴权开关。
const state = reactive({
  initialized: false,
  enabled: true,
  me: null, // 当前主体 {kind,id,scopes,mustChangePassword}
})

async function refresh() {
  const { enabled } = await api.authStatus()
  state.enabled = enabled
  if (!enabled) {
    state.me = null
    state.initialized = true
    return
  }
  try {
    state.me = await api.me()
  } catch {
    state.me = null
  }
  state.initialized = true
}

async function login(username, password) {
  const res = await api.login({ username, password: await encryptField(password) })
  setToken(res.token)
  state.me = { kind: 'user', id: res.id, scopes: res.scopes, mustChangePassword: res.mustChangePassword }
  return res
}

async function changePassword(oldPassword, newPassword) {
  await api.changePassword({
    oldPassword: await encryptField(oldPassword),
    newPassword: await encryptField(newPassword),
  })
  if (state.me) state.me.mustChangePassword = false
}

async function logout() {
  try {
    await api.logout()
  } catch {
    // 忽略登出失败(如已过期),继续本地清理
  }
  setToken('')
  state.me = null
}

export default {
  state,
  refresh,
  login,
  changePassword,
  logout,
}
