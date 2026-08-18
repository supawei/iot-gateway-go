<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Odometer, Link, Cpu, Upload, Key, SwitchButton } from '@element-plus/icons-vue'
import api from './api'
import auth from './stores/auth'

const route = useRoute()
const router = useRouter()
const online = ref(false)
const version = ref(null)
let timer = null

// 登录/改密等全屏页无侧边栏
const isPublic = computed(() => route.meta.public === true || route.meta.bare === true)

// 侧边栏版本行(悬停显示完整构建信息)
const versionLine = computed(() => (version.value ? `v${version.value.version}` : '—'))
const versionDetail = computed(() =>
  version.value
    ? `${version.value.name} ${version.value.version} · commit ${version.value.commit} · built ${version.value.buildTime} · ${version.value.goVersion}`
    : '',
)

async function checkApi() {
  try {
    await api.listStatus()
    online.value = true
  } catch {
    online.value = false
  }
}

async function loadVersion() {
  try {
    version.value = await api.getVersion()
  } catch {
    // 版本接口失败不阻塞界面(后端过旧等场景),侧边栏显示占位 —。
  }
}

async function logout() {
  await auth.logout()
  router.replace('/login')
}

onMounted(() => {
  loadVersion()
  if (!auth.state.enabled) return
  checkApi()
  timer = setInterval(checkApi, 10000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <el-container class="app">
    <el-aside v-if="!isPublic" width="220px">
      <div class="sidebar">
        <div class="brand">
          <span class="brand-mark">◆</span>
          <span class="brand-name">IoT 网关</span>
        </div>
        <el-menu :default-active="route.path" router class="side-menu">
          <el-menu-item index="/dashboard">
            <el-icon><Odometer /></el-icon>
            <span>概览</span>
          </el-menu-item>
          <el-menu-item index="/connections">
            <el-icon><Link /></el-icon>
            <span>连接</span>
          </el-menu-item>
          <el-menu-item index="/devices">
            <el-icon><Cpu /></el-icon>
            <span>设备</span>
          </el-menu-item>
          <el-menu-item index="/outputs">
            <el-icon><Upload /></el-icon>
            <span>北向输出</span>
          </el-menu-item>
          <el-menu-item index="/clients">
            <el-icon><Key /></el-icon>
            <span>三方授权</span>
          </el-menu-item>
        </el-menu>
        <div class="side-foot">
          <div class="side-version" :title="versionDetail">{{ versionLine }}</div>
          <div class="side-row">
            <span class="dot" :class="{ ok: online }"></span>
            <span>{{ online ? '后端已连接' : '后端未连接' }}</span>
            <span class="side-user">{{ auth.state.me?.id || '' }}</span>
            <el-icon class="side-logout" title="退出登录" @click="logout"><SwitchButton /></el-icon>
          </div>
        </div>
      </div>
    </el-aside>
    <el-main class="main" :class="{ 'main-public': isPublic }">
      <router-view />
    </el-main>
  </el-container>
</template>

<style scoped>
.app {
  height: 100%;
}
.main-public {
  padding: 0;
}
.side-user {
  margin-left: auto;
  color: #6b7280;
}
.side-logout {
  cursor: pointer;
  color: #9aa1ac;
}
.side-logout:hover {
  color: #2563eb;
}
</style>
