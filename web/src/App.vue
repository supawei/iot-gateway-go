<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import { useRoute } from 'vue-router'
import { Odometer, Link, Cpu } from '@element-plus/icons-vue'
import api from './api'

const route = useRoute()
const online = ref(false)
let timer = null

async function checkApi() {
  try {
    await api.listStatus()
    online.value = true
  } catch {
    online.value = false
  }
}

onMounted(() => {
  checkApi()
  timer = setInterval(checkApi, 10000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <el-container class="app">
    <el-aside width="220px">
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
        </el-menu>
        <div class="side-foot">
          <span class="dot" :class="{ ok: online }"></span>
          <span>{{ online ? '后端已连接' : '后端未连接' }}</span>
        </div>
      </div>
    </el-aside>
    <el-main class="main">
      <router-view />
    </el-main>
  </el-container>
</template>

<style scoped>
.app {
  height: 100%;
}
</style>
