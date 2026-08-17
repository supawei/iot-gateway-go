<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import api from '../api'
import SchemaForm from '../components/SchemaForm.vue'
import { defaultModel, modelFromValue, valueFromModel } from '../utils/schema'

const list = ref([])
const drivers = ref([])
const loading = ref(false)
const dialogVisible = ref(false)
const editingId = ref('')

const form = reactive({ name: '', driver: '' })
const configModel = ref({})

const driverMap = computed(() => {
  const m = {}
  for (const d of drivers.value) m[d.name] = d
  return m
})

const currentSchema = computed(() => driverMap.value[form.driver]?.config || [])

async function load() {
  loading.value = true
  try {
    const [c, d] = await Promise.all([api.listConnections(), api.listDrivers()])
    list.value = c
    drivers.value = d
  } finally {
    loading.value = false
  }
}

function resetConfig(driver) {
  form.driver = driver
  configModel.value = defaultModel(currentSchema.value)
}

function openCreate() {
  editingId.value = ''
  form.name = ''
  const first = drivers.value[0]?.name || ''
  resetConfig(first)
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
  form.name = row.name
  form.driver = row.driver
  configModel.value = modelFromValue(driverMap.value[row.driver]?.config || [], row.config)
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
  const payload = { name: form.name, driver: form.driver, config }
  try {
    if (editingId.value) {
      await api.updateConnection(editingId.value, payload)
    } else {
      await api.createConnection(payload)
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
    await ElMessageBox.confirm(`确定删除连接「${row.name || row.id}」吗？`, '删除确认', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.deleteConnection(row.id)
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
      <h1>连接</h1>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建连接</el-button>
    </div>

    <div class="panel">
      <el-table v-loading="loading" :data="list" empty-text="暂无连接">
        <el-table-column prop="name" label="名称" min-width="150" />
        <el-table-column label="驱动" width="150">
          <template #default="{ row }">
            <el-tag effect="plain">{{ row.driver }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="配置" min-width="240">
          <template #default="{ row }">
            <span class="mono" style="color: #6b7280">{{ JSON.stringify(row.config ?? {}) }}</span>
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

    <el-dialog
      v-model="dialogVisible"
      :title="editingId ? '编辑连接' : '新建连接'"
      width="560px"
      destroy-on-close
    >
      <el-form label-width="90px">
        <el-form-item label="名称">
          <el-input v-model="form.name" placeholder="车间 Modbus TCP" />
        </el-form-item>
        <el-form-item label="驱动" required>
          <el-select v-model="form.driver" style="width: 100%" @change="resetConfig">
            <el-option v-for="d in drivers" :key="d.name" :label="d.name" :value="d.name" />
          </el-select>
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
