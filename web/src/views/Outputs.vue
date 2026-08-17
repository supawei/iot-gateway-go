<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import api from '../api'
import SchemaForm from '../components/SchemaForm.vue'
import { defaultModel, modelFromValue, valueFromModel } from '../utils/schema'

const list = ref([])
const types = ref([])
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

// 列表里给出一个不含凭据的简短摘要(如 broker/url)。
function summary(row) {
  const c = row.config || {}
  return c.broker || c.url || c.endpoint || ''
}

async function load() {
  loading.value = true
  try {
    const [o, t] = await Promise.all([api.listOutputs(), api.listOutputTypes()])
    list.value = o
    types.value = t
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
      <el-button type="primary" :icon="Plus" @click="openCreate">新建输出</el-button>
    </div>

    <div class="panel">
      <p class="form-hint" style="margin-top: 0">
        配置数据上送目标(MQTT / ThingsBoard / TDengine);保存后立即热重载,无需重启网关。
      </p>
      <el-table v-loading="loading" :data="list" empty-text="暂无输出">
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
        <SchemaForm :schema="currentSchema" :model="configModel" />
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
