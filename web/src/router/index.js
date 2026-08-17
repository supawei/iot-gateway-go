import { createRouter, createWebHistory } from 'vue-router'

import Dashboard from '../views/Dashboard.vue'
import Connections from '../views/Connections.vue'
import Devices from '../views/Devices.vue'
import Clients from '../views/Clients.vue'
import Login from '../views/Login.vue'
import ChangePassword from '../views/ChangePassword.vue'
import auth from '../stores/auth'

const routes = [
  { path: '/login', name: 'login', component: Login, meta: { title: '登录', public: true } },
  { path: '/change-password', name: 'change-password', component: ChangePassword, meta: { title: '修改密码', bare: true } },
  { path: '/', redirect: '/dashboard' },
  { path: '/dashboard', name: 'dashboard', component: Dashboard, meta: { title: '概览' } },
  { path: '/connections', name: 'connections', component: Connections, meta: { title: '连接' } },
  { path: '/devices', name: 'devices', component: Devices, meta: { title: '设备' } },
  { path: '/clients', name: 'clients', component: Clients, meta: { title: '三方授权' } },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

// 鉴权守卫:未初始化先初始化;未登录跳登录;须改密跳改密页。
router.beforeEach(async (to) => {
  if (!auth.state.initialized) {
    await auth.refresh()
  }
  if (to.meta.public) return true
  if (!auth.state.enabled) return true // 鉴权关闭,逃生舱直通
  if (!auth.state.me) {
    return { path: '/login', query: { redirect: to.fullPath } }
  }
  if (auth.state.me.mustChangePassword && to.name !== 'change-password') {
    return { path: '/change-password' }
  }
  return true
})

router.afterEach((to) => {
  document.title = (to.meta.title ? `${to.meta.title} · ` : '') + 'IoT 网关'
})

export default router
