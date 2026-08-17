<script setup>
import { ref, reactive, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, CopyDocument } from '@element-plus/icons-vue'
import api from '../api'

// scope 分组(与后端 docs/authz.md §3.1 矩阵对应),用于友好展示与多选。
const SCOPE_GROUPS = [
  {
    label: '连接',
    options: [
      { value: 'connections:read', label: '读连接' },
      { value: 'connections:write', label: '写连接' },
    ],
  },
  {
    label: '设备',
    options: [
      { value: 'devices:read', label: '读设备' },
      { value: 'devices:write', label: '写设备(配置/点位)' },
      { value: 'devices:command', label: '下发写值(控制)' },
    ],
  },
  {
    label: '运行时数据',
    options: [
      { value: 'status:read', label: '读设备状态' },
      { value: 'values:read', label: '读实时值' },
    ],
  },
  {
    label: '北向输出',
    options: [
      { value: 'outputs:read', label: '读输出配置' },
      { value: 'outputs:write', label: '管理输出配置' },
    ],
  },
  {
    label: '网关',
    options: [
      { value: 'gateway:read', label: '读网关设置' },
      { value: 'gateway:write', label: '修改网关设置' },
    ],
  },
  {
    label: '其他',
    options: [
      { value: 'drivers:read', label: '读驱动列表' },
      { value: 'clients:read', label: '读三方授权' },
      { value: 'clients:write', label: '管理三方授权' },
    ],
  },
]

const SCOPE_LABELS = {}
for (const g of SCOPE_GROUPS) {
  for (const o of g.options) SCOPE_LABELS[o.value] = o.label
}

const list = ref([])
const loading = ref(false)

const dialogVisible = ref(false)
const editingId = ref('')
const form = reactive({ id: '', name: '', scopes: [], enabled: true })

// 新建成功后的 API Key(仅展示一次)
const keyVisible = ref(false)
const newKey = ref('')

async function load() {
  loading.value = true
  try {
    list.value = await api.listClients()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingId.value = ''
  form.id = ''
  form.name = ''
  form.scopes = []
  form.enabled = true
  dialogVisible.value = true
}

function openEdit(row) {
  editingId.value = row.id
  form.id = row.id
  form.name = row.name
  form.scopes = [...(row.scopes ?? [])]
  form.enabled = row.enabled
  dialogVisible.value = true
}

async function save() {
  try {
    if (editingId.value) {
      await api.updateClient(editingId.value, { scopes: form.scopes, enabled: form.enabled })
      ElMessage.success('已保存')
    } else {
      const res = await api.createClient({ id: form.id, name: form.name, scopes: form.scopes })
      newKey.value = res.apiKey
      keyVisible.value = true
    }
    dialogVisible.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(`确定删除三方「${row.name || row.id}」吗？其 API Key 将立即失效。`, '删除确认', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.deleteClient(row.id)
    ElMessage.success('已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function copyKey() {
  try {
    await navigator.clipboard.writeText(newKey.value)
    ElMessage.success('已复制')
  } catch {
    ElMessage.warning('复制失败,请手动选择复制')
  }
}

onMounted(load)
</script>

<template>
  <div>
    <div class="page-head">
      <h1>三方授权</h1>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建三方系统</el-button>
    </div>

    <div class="panel">
      <p class="form-hint" style="margin-top: 0">
        为 MES/SCADA 等三方系统创建 API Key 并按接口授权;API Key 创建后仅展示一次。
      </p>
      <el-table v-loading="loading" :data="list" empty-text="暂无三方系统">
        <el-table-column label="ID" min-width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.id }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="name" label="名称" min-width="150" />
        <el-table-column label="权限" min-width="280">
          <template #default="{ row }">
            <el-tag v-for="s in row.scopes ?? []" :key="s" size="small" effect="plain" style="margin: 2px">
              {{ SCOPE_LABELS[s] || s }}
            </el-tag>
            <span v-if="!(row.scopes?.length)" class="el-text el-text--info" style="font-size: 12px">无权限</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" effect="light" size="small">
              {{ row.enabled ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" min-width="170">
          <template #default="{ row }">
            <span class="el-text el-text--info" style="font-size: 13px">{{ row.createdAt || '—' }}</span>
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
      :title="editingId ? '编辑三方系统' : '新建三方系统'"
      width="560px"
      destroy-on-close
    >
      <el-form label-width="90px">
        <el-form-item label="ID" required>
          <el-input v-model="form.id" :disabled="!!editingId" placeholder="mes-readonly" />
        </el-form-item>
        <el-form-item label="名称">
          <el-input v-model="form.name" placeholder="MES 只读" />
        </el-form-item>
        <el-form-item v-if="editingId" label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
        <el-form-item label="权限">
          <div style="width: 100%">
            <div v-for="g in SCOPE_GROUPS" :key="g.label" class="scope-group">
              <div class="scope-group-label">{{ g.label }}</div>
              <el-checkbox-group v-model="form.scopes">
                <el-checkbox v-for="o in g.options" :key="o.value" :value="o.value">{{ o.label }}</el-checkbox>
              </el-checkbox-group>
            </div>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" @click="save">保存</el-button>
      </template>
    </el-dialog>

    <!-- API Key 仅展示一次 -->
    <el-dialog v-model="keyVisible" title="API Key" width="560px" :close-on-click-modal="false">
      <el-alert type="warning" :closable="false" show-icon title="此 Key 仅展示一次,请立即复制保存" style="margin-bottom: 12px" />
      <div class="key-box">
        <code class="mono">{{ newKey }}</code>
        <el-button :icon="CopyDocument" size="small" @click="copyKey">复制</el-button>
      </div>
      <template #footer>
        <el-button type="primary" @click="keyVisible = false">我已保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.scope-group {
  margin-bottom: 10px;
}
.scope-group-label {
  font-size: 12px;
  color: #9aa1ac;
  margin-bottom: 4px;
}
.scope-group .el-checkbox {
  margin-right: 16px;
}
.key-box {
  display: flex;
  align-items: center;
  gap: 10px;
  background: #f6f7f9;
  border: 1px solid #e7e9ee;
  border-radius: 8px;
  padding: 10px 12px;
}
.key-box code {
  flex: 1;
  word-break: break-all;
  color: #1b1f27;
}
</style>
