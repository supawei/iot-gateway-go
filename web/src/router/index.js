import { createRouter, createWebHistory } from 'vue-router'

import Dashboard from '../views/Dashboard.vue'
import Connections from '../views/Connections.vue'
import Devices from '../views/Devices.vue'

const routes = [
  { path: '/', redirect: '/dashboard' },
  { path: '/dashboard', name: 'dashboard', component: Dashboard, meta: { title: '概览' } },
  { path: '/connections', name: 'connections', component: Connections, meta: { title: '连接' } },
  { path: '/devices', name: 'devices', component: Devices, meta: { title: '设备' } },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.afterEach((to) => {
  document.title = (to.meta.title ? `${to.meta.title} · ` : '') + 'IoT 网关'
})

export default router
