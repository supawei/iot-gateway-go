import { ref } from 'vue'

// 列表前端分页:数据全量加载后按页切分,页码与每页条数可在分页栏中交互选择。
// 用法:
//   const { page, pageSize, pageSizes, total, sync, pageRows, onSizeChange } = usePagination()
//   const pagedList = computed(() => pageRows(list.value))   // 表格 :data 使用
//   数据就绪后调用 sync(list.value);模板内放 el-pagination(见各视图)。
export function usePagination(initialSize = 10) {
  const page = ref(1)
  const pageSize = ref(initialSize)
  const pageSizes = [10, 20, 50, 100]
  const total = ref(0)

  // 每次数据就绪后调用:更新总条数,并自动修正因删除/过滤导致的越界页码。
  function sync(rows) {
    total.value = rows.length
    const maxPage = Math.max(1, Math.ceil(total.value / pageSize.value))
    if (page.value > maxPage) page.value = maxPage
  }

  // 取当前页数据(在 computed 中使用,勿在此处修改页码)。
  function pageRows(rows) {
    const start = (page.value - 1) * pageSize.value
    return rows.slice(start, start + pageSize.value)
  }

  // 切换每页条数后回到第一页。
  function onSizeChange() {
    page.value = 1
  }

  return { page, pageSize, pageSizes, total, sync, pageRows, onSizeChange }
}

export default usePagination
