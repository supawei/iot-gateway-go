<script setup>
import { ref, reactive, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import api from '../api'

const DRIVERS = [
  { value: 'modbus', label: 'Modbus（轮询）' },
  { value: 'modbus_listen', label: 'Modbus 监听' },
  { value: 'opcua', label: 'OPC UA' },
]

const DRIVER_CONFIG_PLACEHOLDER = {
  modbus: '{"mode":"tcp","address":"192.168.1.5:502"}',
  modbus_listen: '{"listen":":502","timeout":"60s"}',
  opcua: '{"endpoint":"opc.tcp://192.168.1.5:4840"}',
}

const list = ref([])
const loading = ref(false)
const dialogVisible = ref(false)
const editingId = ref('')
const formRef = ref()

const form = reactive({
  id: '',
  name: '',
  driver: 'modbus',
  config: '{}',
})

async function load() {
  loading.value = true
  try {
    list.value = await api.listConnections()
  } finally {
    loading.value = false
  }
}

function onDriverChange() {
  form.config = DRIVER_CONFIG_PLACEHOLDER[form.driver] || '{}'
}

function openCreate() {
  editingId.value = ''
  Object.assign(form, { id: '', name: '', driver: 'modbus', config: DRIVER_CONFIG_PLACEHOLDER.modbus })
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
  form.id = row.id
  form.name = row.name
  form.driver = row.driver
  form.config = JSON.stringify(row.config ?? {}, null, 2)
  dialogVisible.value = true
}

async function save() {
  let config
  try {
    config = JSON.parse(form.config)
  } catch {
    ElMessage.error('配置不是合法的 JSON')
    return
  }
  const payload = { id: form.id, name: form.name, driver: form.driver, config }
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
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
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
        <el-table-column label="ID" min-width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.id }}</span>
          </template>
        </el-table-column>
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
      width="520px"
      destroy-on-close
    >
      <el-form ref="formRef" :model="form" label-width="80px">
        <el-form-item label="ID" required>
          <el-input v-model="form.id" :disabled="!!editingId" placeholder="conn-1" />
        </el-form-item>
        <el-form-item label="名称">
          <el-input v-model="form.name" placeholder="车间 Modbus TCP" />
        </el-form-item>
        <el-form-item label="驱动" required>
          <el-select v-model="form.driver" style="width: 100%" @change="onDriverChange">
            <el-option v-for="d in DRIVERS" :key="d.value" :label="d.label" :value="d.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="配置">
          <el-input v-model="form.config" type="textarea" :rows="6" class="mono" />
          <div class="form-hint">传输参数 JSON，具体字段见 README「驱动」章节</div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
