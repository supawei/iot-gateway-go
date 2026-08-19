<script setup>
import { ref, reactive, computed, watch, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Refresh } from '@element-plus/icons-vue'
import api from '../api'
import { usePagination } from '../utils/pagination'

const list = ref([])
const outputs = ref([])
const devices = ref([])
const loading = ref(false)
const dialogVisible = ref(false)
const editingId = ref('')

const { page, pageSize, pageSizes, total, sync, pageRows, onSizeChange } = usePagination()
const pagedList = computed(() => pageRows(list.value))

const form = reactive({
  name: '', enabled: true, severity: 'warning',
  refKeys: [], conds: [], joinOps: [], outputIds: [],
  freshnessSeconds: 300, cooldownSeconds: 0,
  expr: '', advanced: false,
})

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
function fromKey(k) {
  const idx = k.indexOf('|')
  return { deviceId: k.slice(0, idx), point: k.slice(idx + 1) }
}
function fromKeys(keys) { return keys.map(fromKey) }
function toKeys(refs) { return (refs || []).map(keyOf) }
function pointLabel(k) {
  const { deviceId, point } = fromKey(k)
  return `point("${deviceId}","${point}")`
}

const compiledExpr = computed(() => {
  if (form.advanced) return (form.expr || '').trim()
  const segments = form.refKeys.map((k, i) => {
    const cond = (form.conds[i] || '').trim()
    return `${pointLabel(k)}${cond}`
  })
  return segments.map((seg, i) => {
    if (i >= segments.length - 1) return seg
    const op = form.joinOps[i] || '&&'
    return `${seg} ${op}`
  }).join(' ')
})

// 顶层按 && / || 分段反解;含括号(嵌套)或段非 point() 开头则判为复杂表达式,回退高级模式
function parseExpr(expr) {
  const trimmed = (expr || '').trim()
  if (!trimmed || /[()]/.test(trimmed)) return null
  const tokens = trimmed.split(/\s*(\|\||&&)\s*/)
  if (!tokens.length || tokens.length % 2 === 0) return null
  const refKeys = [], conds = [], joinOps = []
  for (let i = 0; i < tokens.length; i += 2) {
    const seg = tokens[i].trim()
    const m = /^point\(\s*"([^"]+)"\s*,\s*"([^"]+)"\s*\)(.*)$/.exec(seg)
    if (!m) return null
    refKeys.push(`${m[1]}|${m[2]}`)
    conds.push(m[3].trim())
    if (i + 1 < tokens.length) joinOps.push(tokens[i + 1])
  }
  return { refKeys, conds, joinOps }
}

function syncConds(len) {
  const conds = form.conds.slice(0, len)
  while (conds.length < len) conds.push('')
  const joinOps = form.joinOps.slice(0, len)
  while (joinOps.length < len) joinOps.push('&&')
  form.conds = conds
  form.joinOps = joinOps
}

watch(() => form.refKeys.slice(), (next) => syncConds(next.length))

async function load() {
  loading.value = true
  try {
    const [r, o, d] = await Promise.all([api.listAlertRules(), api.listOutputs(), api.listDevices()])
    list.value = r
    sync(list.value)
    outputs.value = o
    devices.value = d
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function resetForm() {
  Object.assign(form, {
    name: '', enabled: true, severity: 'warning',
    refKeys: [], conds: [], joinOps: [], outputIds: [],
    freshnessSeconds: 300, cooldownSeconds: 0,
    expr: '', advanced: false,
  })
}

function openCreate() {
  editingId.value = ''
  resetForm()
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
  const parsed = parseExpr(row.expr)
  const refKeys = parsed ? parsed.refKeys : toKeys(row.referencedPoints)
  Object.assign(form, {
    name: row.name, enabled: row.enabled, severity: row.severity || 'warning',
    refKeys, outputIds: row.outputIds || [],
    freshnessSeconds: row.freshnessSeconds || 300, cooldownSeconds: row.cooldownSeconds || 0,
    conds: parsed ? parsed.conds : refKeys.map(() => ''),
    joinOps: parsed && parsed.joinOps.length ? parsed.joinOps : refKeys.map(() => '&&'),
    expr: parsed ? '' : (row.expr || ''),
    advanced: !parsed,
  })
  dialogVisible.value = true
}

async function save() {
  const expr = compiledExpr.value
  if (!expr) { ElMessage.error(form.advanced ? '请填写表达式' : '请选择引用点位并填写条件'); return }
  if (form.refKeys.length === 0) { ElMessage.error('请选择至少一个引用点位'); return }
  // 分段模式下每个引用点都必须填比较条件:空条件生成的表达式结果不是布尔,引擎会跳过该规则。
  if (!form.advanced && form.conds.some((c) => !(c || '').trim())) {
    ElMessage.error('请为每个引用点位填写条件(如 > 30、== "off")')
    return
  }
  const payload = {
    name: form.name, enabled: form.enabled, severity: form.severity,
    expr, referencedPoints: fromKeys(form.refKeys),
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
        配置时选择引用点位后自动生成 point(),只需为每个点位填写条件;复杂场景可切高级模式直接编表达式。
      </p>
      <el-table v-loading="loading" :data="pagedList" empty-text="暂无告警规则">
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

    <el-dialog v-model="dialogVisible" :title="editingId ? '编辑规则' : '新建规则'" width="640px" destroy-on-close>
      <el-form label-width="100px">
        <el-form-item label="名称"><el-input v-model="form.name" placeholder="高温且空调关闭" /></el-form-item>
        <el-form-item label="级别">
          <el-select v-model="form.severity" style="width: 100%" :teleported="true" popper-class="dlg-popper">
            <el-option v-for="s in severityOptions" :key="s.value" :label="s.label" :value="s.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="引用点位" required>
          <el-select v-model="form.refKeys" multiple filterable style="width: 100%" :teleported="true" popper-class="dlg-popper" placeholder="选择表达式引用的设备点位">
            <el-option v-for="o in pointOptions" :key="o.value" :label="o.label" :value="o.value" />
          </el-select>
        </el-form-item>
        <el-form-item v-if="!form.advanced" label="触发条件" required>
          <div v-if="form.refKeys.length === 0" class="cond-empty">先选择引用点位,再为每个点位填写条件</div>
          <div v-for="(k, i) in form.refKeys" :key="k" class="cond-row">
            <span class="mono cond-point" :title="pointLabel(k)">{{ pointLabel(k) }}</span>
            <el-input v-model="form.conds[i]" placeholder="如 > 30" class="cond-input" />
            <el-select v-if="i < form.refKeys.length - 1" v-model="form.joinOps[i]" class="cond-op" :teleported="true" popper-class="dlg-popper">
              <el-option label="且 (AND)" value="&&" />
              <el-option label="或 (OR)" value="||" />
            </el-select>
          </div>
          <p class="form-hint">
            填点位后的比较,如 &gt; 30、&lt;= 20、== "off"、!= "on";每个引用点都必须填写,
            表达式需可求值为 true/false。
          </p>
        </el-form-item>
        <el-form-item v-else label="表达式" required>
          <el-input v-model="form.expr" type="textarea" :rows="3" placeholder='point("d1","temp")>30 && point("d2","sw")=="off"' />
        </el-form-item>
        <el-form-item v-if="!form.advanced && compiledExpr" label="表达式预览">
          <code class="mono cond-preview">{{ compiledExpr }}</code>
        </el-form-item>
        <el-form-item>
          <el-checkbox v-model="form.advanced">高级模式:直接编辑完整表达式</el-checkbox>
        </el-form-item>
        <el-form-item label="投递输出">
          <el-select v-model="form.outputIds" multiple style="width: 100%" :teleported="true" popper-class="dlg-popper" placeholder="告警发往哪些输出">
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

<style scoped>
.cond-row { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
/* 点位标签:长 ID/点名为 nowrap 且不可压缩,会撑破整行、挤压输入框与 且/或 选择框;
   改为限宽省略号 + title 悬停看完整值,保证输入框与选择框始终可见 */
.cond-point {
  color: #6b7280;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 42%;
  flex-shrink: 0;
}
.cond-input { flex: 1 1 auto; min-width: 0; }
.cond-op { width: 110px; flex-shrink: 0; }
.cond-empty { color: #9ca3af; font-size: 13px; }
.cond-preview {
  display: block; background: #f3f4f6; padding: 8px 10px;
  border-radius: 4px; color: #374151; word-break: break-all;
}
</style>
