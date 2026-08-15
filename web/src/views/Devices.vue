<script setup>
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import api from '../api'
import SchemaForm from '../components/SchemaForm.vue'
import { defaultModel, modelFromValue, valueFromModel } from '../utils/schema'

const DATA_TYPES = [
  'bool', 'int16', 'uint16', 'int32', 'uint32', 'int64',
  'float32', 'float64', 'string',
]

const list = ref([])
const connections = ref([])
const drivers = ref([])
const statuses = ref([])
const loading = ref(false)

const dialogVisible = ref(false)
const editingId = ref('')
const form = reactive({
  id: '',
  name: '',
  connectionId: '',
  intervalMs: 1000,
  enabled: true,
  points: [],
})
const paramsModel = ref({})

const statusMap = computed(() => {
  const m = {}
  for (const s of statuses.value) m[s.deviceId] = s
  return m
})

const driverMap = computed(() => {
  const m = {}
  for (const d of drivers.value) m[d.name] = d
  return m
})

const connectionDriver = computed(() => {
  const c = connections.value.find((x) => x.id === form.connectionId)
  return c?.driver || ''
})

const paramSchema = computed(() => driverMap.value[connectionDriver.value]?.params || [])

async function load() {
  loading.value = true
  try {
    const [d, c, s, drv] = await Promise.all([
      api.listDevices(),
      api.listConnections(),
      api.listStatus(),
      api.listDrivers(),
    ])
    list.value = d
    connections.value = c
    statuses.value = s
    drivers.value = drv
  } finally {
    loading.value = false
  }
}

function addPoint() {
  form.points.push({ name: '', address: '', dataType: 'int16', scale: 0 })
}

function removePoint(i) {
  form.points.splice(i, 1)
}

function resetParams() {
  paramsModel.value = defaultModel(paramSchema.value)
}

function openCreate() {
  editingId.value = ''
  Object.assign(form, {
    id: '', name: '', connectionId: connections.value[0]?.id || '',
    intervalMs: 1000, enabled: true, points: [],
  })
  resetParams()
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
  form.id = row.id
  form.name = row.name
  form.connectionId = row.connectionId
  form.intervalMs = row.intervalMs
  form.enabled = row.enabled
  form.points = (row.points ?? []).map((p) => ({
    name: p.name, address: p.address, dataType: p.dataType, scale: p.scale,
  }))
  paramsModel.value = modelFromValue(paramSchema.value, row.params)
  dialogVisible.value = true
}

async function save() {
  let params
  try {
    params = valueFromModel(paramSchema.value, paramsModel.value)
  } catch (e) {
    ElMessage.error(e.message)
    return
  }
  const payload = {
    id: form.id,
    name: form.name,
    connectionId: form.connectionId,
    intervalMs: Number(form.intervalMs) || 0,
    enabled: form.enabled,
    params,
    points: form.points.map((p) => ({
      name: p.name, address: p.address, dataType: p.dataType, scale: Number(p.scale) || 0,
    })),
  }
  try {
    if (editingId.value) {
      await api.updateDevice(editingId.value, payload)
    } else {
      await api.createDevice(payload)
    }
    ElMessage.success('已保存')
    dialogVisible.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(`确定删除设备「${row.name || row.id}」吗？`, '删除确认', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.deleteDevice(row.id)
    ElMessage.success('已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// ---- 克隆 ----
const cloneVisible = ref(false)
const cloneSource = ref(null)
const cloneForm = reactive({ id: '', name: '' })

function openClone(row) {
  cloneSource.value = row
  cloneForm.id = ''
  cloneForm.name = ''
  cloneVisible.value = true
}

async function doClone() {
  try {
    await api.cloneDevice(cloneSource.value.id, { id: cloneForm.id, name: cloneForm.name })
    ElMessage.success('已克隆')
    cloneVisible.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// ---- 属性值 ----
const valuesVisible = ref(false)
const valuesTarget = ref(null)
const valuesList = ref({ points: [] })
const valuesLoading = ref(false)
let valuesTimer = null

function fmtValue(v) {
  if (v === null || v === undefined) return '—'
  if (typeof v === 'object') return JSON.stringify(v)
  return String(v)
}

function fmtTime(t) {
  if (!t || t.startsWith('0001')) return '—'
  return new Date(t).toLocaleString()
}

async function loadValues() {
  if (!valuesTarget.value) return
  valuesLoading.value = true
  try {
    valuesList.value = await api.getDeviceValues(valuesTarget.value.id)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    valuesLoading.value = false
  }
}

function openValues(row) {
  valuesTarget.value = row
  valuesVisible.value = true
  loadValues()
  valuesTimer = setInterval(loadValues, 3000)
}

function closeValues() {
  if (valuesTimer) clearInterval(valuesTimer)
  valuesTimer = null
}

// ---- 写值 ----
const writeVisible = ref(false)
const writeTarget = ref(null)
const writeForm = reactive({ point: '', value: '' })

function openWrite(row) {
  writeTarget.value = row
  writeForm.point = row.points?.[0]?.name || ''
  writeForm.value = ''
  writeVisible.value = true
}

function parseValue(s) {
  if (s === 'true') return true
  if (s === 'false') return false
  if (s !== '' && !Number.isNaN(Number(s))) return Number(s)
  return s
}

async function doWrite() {
  try {
    await api.writeDevice(writeTarget.value.id, {
      point: writeForm.point,
      value: parseValue(writeForm.value),
    })
    ElMessage.success('已下发')
    writeVisible.value = false
  } catch (e) {
    ElMessage.error(e.message)
  }
}

onMounted(load)
onUnmounted(() => {
  if (valuesTimer) clearInterval(valuesTimer)
})
</script>

<template>
  <div>
    <div class="page-head">
      <h1>设备</h1>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建设备</el-button>
    </div>

    <div class="panel">
      <el-table v-loading="loading" :data="list" empty-text="暂无设备">
        <el-table-column label="ID" min-width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.id }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column label="连接" min-width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.connectionId }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="100">
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
        <el-table-column prop="intervalMs" label="周期(ms)" width="100" />
        <el-table-column label="点位" width="80">
          <template #default="{ row }">{{ row.points?.length ?? 0 }}</template>
        </el-table-column>
        <el-table-column label="启用" width="80">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" effect="plain" size="small">
              {{ row.enabled ? '是' : '否' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="280" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button link type="primary" @click="openValues(row)">属性值</el-button>
            <el-button link type="primary" @click="openClone(row)">克隆</el-button>
            <el-button link type="warning" @click="openWrite(row)">写值</el-button>
            <el-button link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 设备编辑对话框 -->
    <el-dialog
      v-model="dialogVisible"
      :title="editingId ? '编辑设备' : '新建设备'"
      width="640px"
      destroy-on-close
      top="5vh"
    >
      <el-form :model="form" label-width="90px">
        <el-row :gutter="14">
          <el-col :span="12">
            <el-form-item label="ID" required>
              <el-input v-model="form.id" :disabled="!!editingId" placeholder="sensor-01" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="名称">
              <el-input v-model="form.name" placeholder="温湿度传感器" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="14">
          <el-col :span="12">
            <el-form-item label="连接" required>
              <el-select v-model="form.connectionId" style="width: 100%" placeholder="选择连接" @change="resetParams">
                <el-option v-for="c in connections" :key="c.id" :label="`${c.id} (${c.driver})`" :value="c.id" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="周期(ms)">
              <el-input-number v-model="form.intervalMs" :min="0" style="width: 100%" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>

        <!-- 设备参数:按所选连接的驱动 schema 动态渲染 -->
        <template v-if="paramSchema.length">
          <el-divider content-position="left">设备参数</el-divider>
          <SchemaForm :schema="paramSchema" :model="paramsModel" />
        </template>

        <el-divider content-position="left">点位</el-divider>
        <el-form-item label="点位">
          <div style="width: 100%">
            <div class="point-row" style="margin-bottom: 4px; font-size: 12px; color: #9aa1ac">
              <span>名称</span><span>地址</span><span>类型</span><span>系数</span><span></span>
            </div>
            <div v-for="(p, i) in form.points" :key="i" class="point-row">
              <el-input v-model="p.name" placeholder="temperature" />
              <el-input v-model="p.address" placeholder="holding:0" />
              <el-select v-model="p.dataType">
                <el-option v-for="t in DATA_TYPES" :key="t" :label="t" :value="t" />
              </el-select>
              <el-input-number v-model="p.scale" :step="0.1" :controls="false" />
              <el-button link type="danger" @click="removePoint(i)">✕</el-button>
            </div>
            <el-button text type="primary" :icon="Plus" @click="addPoint">添加点位</el-button>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>

    <!-- 克隆对话框 -->
    <el-dialog v-model="cloneVisible" title="克隆设备" width="420px" destroy-on-close>
      <el-form :model="cloneForm" label-width="70px">
        <el-form-item label="新 ID" required>
          <el-input v-model="cloneForm.id" placeholder="sensor-02" />
        </el-form-item>
        <el-form-item label="名称" required>
          <el-input v-model="cloneForm.name" placeholder="温湿度-2" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="cloneVisible = false">取消</el-button>
        <el-button type="primary" @click="doClone">克隆</el-button>
      </template>
    </el-dialog>

    <!-- 属性值对话框 -->
    <el-dialog
      v-model="valuesVisible"
      :title="`属性值 · ${valuesTarget?.name || valuesTarget?.id || ''}`"
      width="640px"
      destroy-on-close
      @closed="closeValues"
    >
      <el-table v-loading="valuesLoading" :data="valuesList.points" empty-text="暂无采集数据">
        <el-table-column prop="point" label="点位" min-width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.point }}</span>
          </template>
        </el-table-column>
        <el-table-column label="值" min-width="160">
          <template #default="{ row }">
            <span class="mono">{{ fmtValue(row.value) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="质量" width="100">
          <template #default="{ row }">
            <el-tag
              :type="row.quality === 'good' ? 'success' : row.quality === 'uncertain' ? 'warning' : 'danger'"
              effect="light"
              size="small"
            >
              {{ row.quality }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="时间" min-width="170">
          <template #default="{ row }">
            <span class="el-text el-text--info" style="font-size: 13px">{{ fmtTime(row.timestamp) }}</span>
          </template>
        </el-table-column>
      </el-table>
      <template #footer>
        <el-button @click="loadValues">刷新</el-button>
        <el-button type="primary" @click="valuesVisible = false">关闭</el-button>
      </template>
    </el-dialog>

    <!-- 写值对话框 -->
    <el-dialog v-model="writeVisible" title="写值" width="420px" destroy-on-close>
      <el-form :model="writeForm" label-width="70px">
        <el-form-item label="点位" required>
          <el-select v-model="writeForm.point" style="width: 100%">
            <el-option v-for="p in writeTarget?.points || []" :key="p.name" :label="p.name" :value="p.name" />
          </el-select>
        </el-form-item>
        <el-form-item label="值" required>
          <el-input v-model="writeForm.value" placeholder="42 / true / hello" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="writeVisible = false">取消</el-button>
        <el-button type="primary" @click="doWrite">下发</el-button>
      </template>
    </el-dialog>
  </div>
</template>
