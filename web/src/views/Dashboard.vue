<script setup>
import { ref, computed, onMounted } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import api from '../api'

const devices = ref([])
const connections = ref([])
const statuses = ref([])
const loading = ref(false)

const statusMap = computed(() => {
  const m = {}
  for (const s of statuses.value) m[s.deviceId] = s
  return m
})

const onlineCount = computed(() => statuses.value.filter((s) => s.online).length)

async function load() {
  loading.value = true
  try {
    const [d, c, s] = await Promise.all([
      api.listDevices(),
      api.listConnections(),
      api.listStatus(),
    ])
    devices.value = d
    connections.value = c
    statuses.value = s
  } finally {
    loading.value = false
  }
}

function fmtTime(t) {
  if (!t || t.startsWith('0001')) return '—'
  return new Date(t).toLocaleString()
}

onMounted(load)
</script>

<template>
  <div>
    <div class="page-head">
      <h1>概览</h1>
      <el-button :icon="Refresh" @click="load">刷新</el-button>
    </div>

    <div class="stat-cards">
      <div class="stat">
        <div class="k">设备总数</div>
        <div class="v">{{ devices.length }}</div>
      </div>
      <div class="stat">
        <div class="k">在线设备</div>
        <div class="v green">{{ onlineCount }}</div>
      </div>
      <div class="stat">
        <div class="k">连接数</div>
        <div class="v">{{ connections.length }}</div>
      </div>
    </div>

    <div class="panel">
      <h2 class="panel-title">设备状态</h2>
      <el-table v-loading="loading" :data="devices" empty-text="暂无设备">
        <el-table-column prop="id" label="设备 ID" min-width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.id }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag
              v-if="statusMap[row.id]"
              :type="statusMap[row.id].online ? 'success' : 'danger'"
              effect="light"
              round
            >
              {{ statusMap[row.id].online ? '在线' : '离线' }}
            </el-tag>
            <el-tag v-else type="info" effect="light" round>未知</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="最近采集" min-width="170">
          <template #default="{ row }">
            {{ statusMap[row.id] ? fmtTime(statusMap[row.id].lastCollect) : '—' }}
          </template>
        </el-table-column>
        <el-table-column label="最近错误" min-width="220">
          <template #default="{ row }">
            <span class="el-text el-text--info" style="font-size: 13px">
              {{ statusMap[row.id]?.lastError || '—' }}
            </span>
          </template>
        </el-table-column>
      </el-table>
    </div>
  </div>
</template>
