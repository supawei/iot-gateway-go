<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Refresh } from '@element-plus/icons-vue'
import api from '../api'
import SchemaForm from '../components/SchemaForm.vue'
import { defaultModel, modelFromValue, valueFromModel } from '../utils/schema'
import { encryptSensitive } from '../utils/crypto'

const list = ref([])
const types = ref([])
const statuses = ref([])
const loading = ref(false)

const dialogVisible = ref(false)
const editingId = ref('')

const form = reactive({ name: '', type: '', enabled: true })
const configModel = ref({})

const typeMap = computed(() => {
  const m = {}
  for (const t of types.value) m[t.type] = t
  return m
})

const currentSchema = computed(() => typeMap.value[form.type]?.schema || [])

const statusMap = computed(() => {
  const m = {}
  for (const s of statuses.value) m[s.outputId] = s
  return m
})

// 取该输出行对应的运行态(无则空对象)。
function st(row) {
  return statusMap.value[row.id] || {}
}

// 连接状态 → 标签文案/颜色。
function connState(s) {
  if (!s || !s.active) return { label: '未启用', type: 'info' }
  if (s.connected && s.connectionOpen) return { label: '已连接', type: 'success' }
  if (s.connected) return { label: '重连中', type: 'warning' }
  return { label: '未连接', type: 'danger' }
}

function fmtTime(t) {
  if (!t || t.startsWith('0001')) return '—'
  return new Date(t).toLocaleString()
}

// 列表里给出一个不含凭据的简短摘要(如 broker/url)。
function summary(row) {
  const c = row.config || {}
  return c.broker || c.url || c.endpoint || ''
}

async function load() {
  loading.value = true
  try {
    const [o, t, s] = await Promise.all([api.listOutputs(), api.listOutputTypes(), api.listOutputStatus()])
    list.value = o
    types.value = t
    statuses.value = s
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function resetConfig(type) {
  form.type = type
  configModel.value = defaultModel(currentSchema.value)
}

function openCreate() {
  editingId.value = ''
  form.name = ''
  form.enabled = true
  const first = types.value[0]?.type || ''
  resetConfig(first)
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
  form.name = row.name
  form.type = row.type
  form.enabled = row.enabled
  configModel.value = modelFromValue(typeMap.value[row.type]?.schema || [], row.config)
  dialogVisible.value = true
}

async function save() {
  let config
  try {
    config = valueFromModel(currentSchema.value, configModel.value)
    config = await encryptSensitive(config, currentSchema.value)
  } catch (e) {
    ElMessage.error(e.message)
    return
  }
  const payload = { name: form.name, type: form.type, enabled: form.enabled, config }
  try {
    if (editingId.value) {
      await api.updateOutput(editingId.value, payload)
    } else {
      await api.createOutput(payload)
    }
    ElMessage.success('已保存,输出已热重载')
    dialogVisible.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(`确定删除输出「${row.name || row.id}」吗？删除后立即停止上送。`, '删除确认', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.deleteOutput(row.id)
    ElMessage.success('已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

onMounted(load)
</script>

<template>
  <div>
    <div class="page-head">
      <h1>北向输出</h1>
      <el-button :icon="Refresh" @click="load">刷新</el-button>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建输出</el-button>
    </div>

    <div class="panel">
      <p class="form-hint" style="margin-top: 0">
        配置数据上送目标(MQTT / ThingsBoard / TDengine);保存后立即热重载,无需重启网关。
        点行展开可查看运行状态(连接、上送、积压与错误)。
      </p>
      <el-table v-loading="loading" :data="list" empty-text="暂无输出">
        <el-table-column type="expand">
          <template #default="{ row }">
            <div style="padding: 4px 24px 12px">
              <el-descriptions :column="3" size="small" border>
                <el-descriptions-item label="队列占用">{{ st(row).queueUsed }} / {{ st(row).queueCap }}</el-descriptions-item>
                <el-descriptions-item label="待发缓冲">{{ st(row).pending }}</el-descriptions-item>
                <el-descriptions-item label="已接收">{{ st(row).received }}</el-descriptions-item>
                <el-descriptions-item label="已丢弃">{{ st(row).dropped }}</el-descriptions-item>
                <el-descriptions-item label="补传队列">{{ st(row).backfill }}</el-descriptions-item>
                <el-descriptions-item label="成功发送">{{ st(row).sent }}</el-descriptions-item>
                <el-descriptions-item label="最近发送">{{ fmtTime(st(row).lastSentAt) }}</el-descriptions-item>
                <el-descriptions-item label="最近错误" :span="3">
                  <span v-if="st(row).lastError" style="color: #f56c6c">{{ st(row).lastError }}</span>
                  <span v-else class="el-text el-text--info">—</span>
                </el-descriptions-item>
              </el-descriptions>
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column label="类型" width="130">
          <template #default="{ row }">
            <el-tag effect="plain">{{ typeMap[row.type]?.label || row.type }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="目标" min-width="200">
          <template #default="{ row }">
            <span class="mono" style="color: #6b7280">{{ summary(row) || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="启用" width="80">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" effect="plain" size="small">
              {{ row.enabled ? '是' : '否' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag :type="connState(st(row)).type" effect="light" round size="small">
              {{ connState(st(row)).label }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="150" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 新建/编辑对话框 -->
    <el-dialog
      v-model="dialogVisible"
      :title="editingId ? '编辑输出' : '新建输出'"
      width="560px"
      destroy-on-close
    >
      <el-form label-width="100px">
        <el-form-item label="名称">
          <el-input v-model="form.name" placeholder="MQTT 主站" />
        </el-form-item>
        <el-form-item label="类型" required>
          <el-select
            v-model="form.type"
            style="width: 100%"
            :disabled="!!editingId"
            @change="resetConfig"
          >
            <el-option v-for="t in types" :key="t.type" :label="t.label" :value="t.type" />
          </el-select>
        </el-form-item>
        <el-form-item v-if="editingId" label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
        <SchemaForm :schema="currentSchema" :model="configModel" :editing="!!editingId" />
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
