<script setup>
import { ref, computed, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { Edit, Refresh } from '@element-plus/icons-vue'
import api from '../api'
import { usePagination } from '../utils/pagination'

const devices = ref([])
const connections = ref([])
const statuses = ref([])
const gatewayId = ref('')
const procStats = ref(null)
const loading = ref(false)

const { page, pageSize, pageSizes, total, sync, pageRows, onSizeChange } = usePagination()
const pagedDevices = computed(() => pageRows(devices.value))

const statusMap = computed(() => {
  const m = {}
  for (const s of statuses.value) m[s.deviceId] = s
  return m
})

const onlineCount = computed(() => statuses.value.filter((s) => s.online).length)

async function load() {
  loading.value = true
  try {
    const [d, c, s, g, p] = await Promise.all([
      api.listDevices(),
      api.listConnections(),
      api.listStatus(),
      api.getGateway(),
      api.getProcessingStatus(),
    ])
    devices.value = d
    sync(devices.value)
    connections.value = c
    statuses.value = s
    gatewayId.value = g.id
    procStats.value = p
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

// ---- 修改网关 ID ----
const editVisible = ref(false)
const editForm = ref({ id: '' })

function openEdit() {
  editForm.value = { id: gatewayId.value }
  editVisible.value = true
}

async function saveGateway() {
  const id = (editForm.value.id || '').trim()
  if (!id) {
    ElMessage.warning('网关 ID 不能为空')
    return
  }
  try {
    const res = await api.updateGateway({ id })
    gatewayId.value = res.id
    ElMessage.success('已保存,输出已按新 ID 热重载')
    editVisible.value = false
  } catch (e) {
    ElMessage.error(e.message)
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
      <div class="stat">
        <div class="k">
          网关 ID
          <el-button link type="primary" :icon="Edit" style="margin-left: 4px" @click="openEdit">改</el-button>
        </div>
        <div class="v mono gw-id" :title="gatewayId">{{ gatewayId || '—' }}</div>
      </div>
      <div class="stat">
        <div class="k">边缘处理</div>
        <div class="v" style="font-size: 16px">
          {{ procStats ? `${procStats.activeRules} 规则 / ${procStats.activeAggregators} 聚合` : '—' }}
        </div>
        <div class="d" style="font-size: 12px; color: #9aa1ac">
          过滤 {{ procStats?.pointsFiltered ?? 0 }} · 聚合产出 {{ procStats?.pointsAggregated ?? 0 }}
        </div>
      </div>
    </div>

    <!-- 修改网关 ID -->
    <el-dialog v-model="editVisible" title="修改网关 ID" width="420px" destroy-on-close>
      <el-form label-width="90px">
        <el-form-item label="网关 ID" required>
          <el-input v-model="editForm.id" placeholder="gw-01" class="mono" />
        </el-form-item>
        <p class="form-hint" style="margin: 0">
          网关 ID 用于 MQTT 输出 topic 等标识,保存后输出立即按新 ID 热重载。
        </p>
      </el-form>
      <template #footer>
        <el-button @click="editVisible = false">取消</el-button>
        <el-button type="primary" @click="saveGateway">保存</el-button>
      </template>
    </el-dialog>

    <div class="panel">
      <h2 class="panel-title">设备状态</h2>
      <el-table v-loading="loading" :data="pagedDevices" empty-text="暂无设备">
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
      <div class="pager">
        <el-pagination
          v-model:current-page="page"
          v-model:page-size="pageSize"
          :page-sizes="pageSizes"
          :total="total"
          layout="total, sizes, prev, pager, next, jumper"
          @size-change="onSizeChange"
        />
      </div>
    </div>
  </div>
</template>
