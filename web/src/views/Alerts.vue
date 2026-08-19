<script setup>
import { ref, computed, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import api from '../api'
import { usePagination } from '../utils/pagination'

const list = ref([])
const loading = ref(false)
const statusFilter = ref('')

const { page, pageSize, pageSizes, total, sync, pageRows, onSizeChange } = usePagination()
const pagedList = computed(() => pageRows(list.value))

async function load() {
  loading.value = true
  try {
    list.value = await api.listAlerts(statusFilter.value)
    sync(list.value)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function fmtTime(t) {
  if (!t || t.startsWith('0001')) return '-'
  return new Date(t).toLocaleString()
}

onMounted(load)
</script>

<template>
  <div>
    <div class="page-head">
      <h1>告警记录</h1>
      <el-select v-model="statusFilter" @change="load" style="width: 140px">
        <el-option label="全部" value="" />
        <el-option label="未解除" value="pending" />
        <el-option label="已解除" value="resolved" />
      </el-select>
      <el-button :icon="Refresh" @click="load">刷新</el-button>
    </div>

    <div class="panel">
      <el-table v-loading="loading" :data="pagedList" empty-text="暂无告警记录">
        <el-table-column label="级别" width="90">
          <template #default="{ row }">
            <el-tag :type="row.severity === 'critical' ? 'danger' : 'warning'" effect="plain" size="small">{{ row.severity }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="ruleName" label="规则" min-width="140" />
        <el-table-column prop="message" label="告警内容" min-width="220" show-overflow-tooltip />
        <el-table-column label="触发时间" width="180">
          <template #default="{ row }">{{ fmtTime(row.triggeredAt) }}</template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="row.status === 'pending' ? 'danger' : 'success'" effect="light" size="small">
              {{ row.status === 'pending' ? '未解除' : '已解除' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="解除时间" width="180">
          <template #default="{ row }">{{ fmtTime(row.resolvedAt) }}</template>
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
