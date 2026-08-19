<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Refresh } from '@element-plus/icons-vue'
import api from '../api'

const list = ref([])
const outputs = ref([])
const devices = ref([])
const loading = ref(false)
const dialogVisible = ref(false)
const editingId = ref('')

const form = reactive({
  name: '', enabled: true, severity: 'warning',
  expr: '', refKeys: [], outputIds: [],
  freshnessSeconds: 300, cooldownSeconds: 0,
})

// 所有设备点位展开为选项(key="devId|point")
const pointOptions = computed(() => {
  const opts = []
  for (const d of devices.value) {
    for (const p of d.points || []) {
      opts.push({ value: `${d.id}|${p.name}`, label: `${d.id} / ${p.name}` })
    }
  }
  return opts
})

const outputOptions = computed(() =>
  outputs.value.map((o) => ({ value: o.id, label: o.name || o.id }))
)

const severityOptions = [
  { value: 'warning', label: '警告' },
  { value: 'critical', label: '严重' },
]

function keyOf(rp) { return `${rp.deviceId}|${rp.point}` }
function fromKeys(keys) {
  return keys.map((k) => {
    const idx = k.indexOf('|')
    return { deviceId: k.slice(0, idx), point: k.slice(idx + 1) }
  })
}
function toKeys(refs) { return (refs || []).map(keyOf) }

async function load() {
  loading.value = true
  try {
    const [r, o, d] = await Promise.all([api.listAlertRules(), api.listOutputs(), api.listDevices()])
    list.value = r
    outputs.value = o
    devices.value = d
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingId.value = ''
  Object.assign(form, {
    name: '', enabled: true, severity: 'warning',
    expr: '', refKeys: [], outputIds: [],
    freshnessSeconds: 300, cooldownSeconds: 0,
  })
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
  Object.assign(form, {
    name: row.name, enabled: row.enabled, severity: row.severity || 'warning',
    expr: row.expr, refKeys: toKeys(row.referencedPoints), outputIds: row.outputIds || [],
    freshnessSeconds: row.freshnessSeconds || 300, cooldownSeconds: row.cooldownSeconds || 0,
  })
  dialogVisible.value = true
}

async function save() {
  if (!form.expr.trim()) { ElMessage.error('请填写表达式'); return }
  if (form.refKeys.length === 0) { ElMessage.error('请选择至少一个引用点位'); return }
  const payload = {
    name: form.name, enabled: form.enabled, severity: form.severity,
    expr: form.expr, referencedPoints: fromKeys(form.refKeys),
    outputIds: form.outputIds, freshnessSeconds: form.freshnessSeconds,
    cooldownSeconds: form.cooldownSeconds,
  }
  try {
    if (editingId.value) await api.updateAlertRule(editingId.value, payload)
    else await api.createAlertRule(payload)
    ElMessage.success('已保存,告警规则已热重载')
    dialogVisible.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(`确定删除告警规则「${row.name || row.id}」吗？`, '删除确认', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch { return }
  try {
    await api.deleteAlertRule(row.id)
    ElMessage.success('已删除')
    load()
  } catch (e) { ElMessage.error(e.message) }
}

function refsSummary(row) {
  return (row.referencedPoints || []).map(keyOf).join(', ')
}

onMounted(load)
</script>

<template>
  <div>
    <div class="page-head">
      <h1>告警规则</h1>
      <el-button :icon="Refresh" @click="load">刷新</el-button>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建规则</el-button>
    </div>

    <div class="panel">
      <p class="form-hint" style="margin-top: 0">
        跨设备/跨点位告警:表达式求值为 true 即边沿触发告警,条件解除自动恢复。
        表达式用 point("设备ID","点位名") 引用点位最新值,如
        point("d1","temp")&gt;30 &amp;&amp; point("d2","sw")=="off"。
      </p>
      <el-table v-loading="loading" :data="list" empty-text="暂无告警规则">
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column label="级别" width="90">
          <template #default="{ row }">
            <el-tag :type="row.severity === 'critical' ? 'danger' : 'warning'" effect="plain" size="small">{{ row.severity }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="引用点位" min-width="200">
          <template #default="{ row }"><span class="mono" style="color: #6b7280">{{ refsSummary(row) }}</span></template>
        </el-table-column>
        <el-table-column prop="expr" label="表达式" min-width="240" show-overflow-tooltip />
        <el-table-column label="输出" min-width="120">
          <template #default="{ row }">{{ (row.outputIds || []).join(', ') || '-' }}</template>
        </el-table-column>
        <el-table-column label="启用" width="80">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" effect="plain" size="small">{{ row.enabled ? '是' : '否' }}</el-tag>
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

    <el-dialog v-model="dialogVisible" :title="editingId ? '编辑规则' : '新建规则'" width="640px" destroy-on-close>
      <el-form label-width="100px">
        <el-form-item label="名称"><el-input v-model="form.name" placeholder="高温且空调关闭" /></el-form-item>
        <el-form-item label="级别">
          <el-select v-model="form.severity" style="width: 100%">
            <el-option v-for="s in severityOptions" :key="s.value" :label="s.label" :value="s.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="引用点位" required>
          <el-select v-model="form.refKeys" multiple filterable style="width: 100%" placeholder="选择表达式引用的设备点位">
            <el-option v-for="o in pointOptions" :key="o.value" :label="o.label" :value="o.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="表达式" required>
          <el-input v-model="form.expr" type="textarea" :rows="3" placeholder='point("d1","temp")>30 && point("d2","sw")=="off"' />
        </el-form-item>
        <el-form-item label="投递输出">
          <el-select v-model="form.outputIds" multiple style="width: 100%" placeholder="告警发往哪些输出">
            <el-option v-for="o in outputOptions" :key="o.value" :label="o.label" :value="o.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="新鲜度(秒)"><el-input-number v-model="form.freshnessSeconds" :min="1" /></el-form-item>
        <el-form-item label="防抖(秒)"><el-input-number v-model="form.cooldownSeconds" :min="0" /></el-form-item>
        <el-form-item v-if="editingId" label="启用"><el-switch v-model="form.enabled" /></el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
