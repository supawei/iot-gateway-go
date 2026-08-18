<script setup>
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import api from '../api'
import SchemaForm from '../components/SchemaForm.vue'
import { defaultModel, modelFromValue, valueFromModel } from '../utils/schema'
import { encryptSensitive } from '../utils/crypto'

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

const connectionNameMap = computed(() => {
  const m = {}
  for (const c of connections.value) m[c.id] = c.name || c.id
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
  form.points.push({ name: '', address: '', dataType: 'int16', scale: 1 })
}

function removePoint(i) {
  form.points.splice(i, 1)
}

// 节点浏览(OPC UA 等支持 Browse 的驱动):点位编辑时"浏览选择"NodeID。
const browseVisible = ref(false)
const browseTarget = ref(null) // 当前在编辑的点位对象,选中的 NodeID 回填到其 address
const browseLoading = ref(false)

// 仅当所选连接的驱动支持 Browse(目前 opcua)时显示浏览按钮。
const canBrowse = computed(() => connectionDriver.value === 'opcua')

function openBrowse(point) {
  browseTarget.value = point
  browseVisible.value = true
}

// el-tree 懒加载:根节点 level=0 传空 parent,展开子节点传 node.data.nodeId。
async function browseLoad(node, resolve) {
  const parent = node.level === 0 ? '' : node.data.nodeId
  browseLoading.value = true
  try {
    const nodes = await api.browseConnection(form.connectionId, parent)
    resolve(nodes.map((n) => ({ ...n, isLeaf: !n.hasChildren })))
  } catch (e) {
    ElMessage.error('浏览失败: ' + e.message)
    resolve([])
  } finally {
    browseLoading.value = false
  }
}

// 归一化节点类型短名:服务端返回 "Variable",双保险兼容 "NodeClassVariable" 等前缀形式。
function shortNodeClass(nodeClass) {
  return String(nodeClass || '').replace(/^NodeClass/, '')
}

function pickNode(data) {
  // OPC UA 只有 Variable 节点承载可读写 Value,Object/Method 仅用于展开导航
  if (shortNodeClass(data.nodeClass) !== 'Variable') {
    return
  }
  if (browseTarget.value) {
    browseTarget.value.address = data.nodeId
    // 服务端带回的 dataType 短名(如 "float64")若在可选类型内,一并回填点位类型
    if (data.dataType && DATA_TYPES.includes(data.dataType)) {
      browseTarget.value.dataType = data.dataType
    }
  }
  browseVisible.value = false
}

function resetParams() {
  paramsModel.value = defaultModel(paramSchema.value)
}

function openCreate() {
  editingId.value = ''
  Object.assign(form, {
    name: '', connectionId: connections.value[0]?.id || '',
    intervalMs: 1000, enabled: true, points: [],
  })
  resetParams()
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
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
    params = await encryptSensitive(params, paramSchema.value)
  } catch (e) {
    ElMessage.error(e.message)
    return
  }
  const payload = {
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
const cloneForm = reactive({ name: '' })

function openClone(row) {
  cloneSource.value = row
  cloneForm.name = `${row.name || row.id}-copy`
  cloneVisible.value = true
}

async function doClone() {
  try {
    await api.cloneDevice(cloneSource.value.id, { name: cloneForm.name })
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
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column label="连接" min-width="140">
          <template #default="{ row }">
            {{ connectionNameMap[row.connectionId] || row.connectionId }}
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
            <el-form-item label="名称" required>
              <el-input v-model="form.name" placeholder="温湿度传感器" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="14">
          <el-col :span="12">
            <el-form-item label="连接" required>
              <el-select v-model="form.connectionId" style="width: 100%" placeholder="选择连接" @change="resetParams">
                <el-option
                  v-for="c in connections"
                  :key="c.id"
                  :label="`${connectionNameMap[c.id]} (${c.driver})`"
                  :value="c.id"
                />
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
          <SchemaForm :schema="paramSchema" :model="paramsModel" :editing="!!editingId" />
        </template>

        <el-divider content-position="left">点位</el-divider>
        <el-form-item label="点位">
          <div style="width: 100%">
            <div class="point-row" style="margin-bottom: 4px; font-size: 12px; color: #9aa1ac">
              <span>名称</span><span>地址</span><span>类型</span><span>系数</span><span></span>
            </div>
            <div v-for="(p, i) in form.points" :key="i" class="point-row">
              <el-input v-model="p.name" placeholder="temperature" />
              <el-input v-model="p.address" :placeholder="canBrowse ? 'ns=2;s=Temperature' : 'holding:0'">
                <template v-if="canBrowse" #append>
                  <el-button link type="primary" @click="openBrowse(p)">浏览</el-button>
                </template>
              </el-input>
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

    <!-- 节点浏览对话框(OPC UA 等支持 Browse 的驱动):懒加载节点树,点击节点回填 address -->
    <el-dialog v-model="browseVisible" title="浏览选择节点" width="520px" destroy-on-close>
      <div class="browse-tip">点击叶子节点(Variable)把 NodeID 填入点位地址;Object 可继续展开。</div>
      <el-tree
        v-loading="browseLoading"
        :props="{ label: 'displayName', isLeaf: 'isLeaf' }"
        node-key="nodeId"
        lazy
        :load="browseLoad"
        highlight-current
        empty-text="无子节点"
        style="max-height: 420px; overflow: auto"
        @node-click="pickNode"
      >
        <template #default="{ data }">
          <span class="mono">{{ data.displayName || data.browseName }}</span>
          <span class="node-class">{{ data.nodeClass }}{{ data.dataType ? ' · ' + data.dataType : '' }}</span>
        </template>
      </el-tree>
      <template #footer>
        <el-button @click="browseVisible = false">取消</el-button>
      </template>
    </el-dialog>

    <!-- 克隆对话框 -->
    <el-dialog v-model="cloneVisible" title="克隆设备" width="420px" destroy-on-close>
      <el-form :model="cloneForm" label-width="70px">
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

<style scoped>
.browse-tip {
  font-size: 12px;
  color: #9aa1ac;
  margin-bottom: 8px;
}
.node-class {
  font-size: 11px;
  color: #909399;
  margin-left: 6px;
}
</style>
