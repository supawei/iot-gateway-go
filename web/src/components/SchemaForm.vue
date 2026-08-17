<script setup>
import { isVisible } from '../utils/schema'

// 通用配置表单:根据驱动声明的 schema 动态渲染字段。
// 需放在 el-form 内部使用;json 字段以文本框编辑 JSON,其余字段用对应控件。
// 字段带 showWhen 时,依赖字段值不匹配则隐藏。
const props = defineProps({
  schema: { type: Array, default: () => [] },
  model: { type: Object, required: true },
})

function visible(f) {
  return isVisible(f, props.model)
}
</script>

<template>
  <template v-for="f in schema" :key="f.name">
    <el-form-item v-if="visible(f)" :label="f.label" :required="f.required">
      <!-- 文本 -->
      <el-input
        v-if="f.type === 'string'"
        v-model="model[f.name]"
        :placeholder="f.placeholder"
        clearable
      />
      <!-- 密码/令牌 -->
      <el-input
        v-else-if="f.type === 'password'"
        v-model="model[f.name]"
        type="password"
        show-password
        :placeholder="f.placeholder"
        autocomplete="new-password"
      />
      <!-- 整数 -->
      <el-input-number
        v-else-if="f.type === 'int'"
        v-model="model[f.name]"
        :step="1"
        step-strictly
        style="width: 100%"
      />
      <!-- 数值 -->
      <el-input-number v-else-if="f.type === 'number'" v-model="model[f.name]" style="width: 100%" />
      <!-- 布尔 -->
      <el-switch v-else-if="f.type === 'bool'" v-model="model[f.name]" />
      <!-- 枚举 -->
      <el-select v-else-if="f.type === 'enum'" v-model="model[f.name]" style="width: 100%">
        <el-option v-for="o in f.options" :key="o" :label="o" :value="o" />
      </el-select>
      <!-- JSON -->
      <el-input
        v-else-if="f.type === 'json'"
        v-model="model[f.name]"
        type="textarea"
        :rows="4"
        class="mono"
      />
      <!-- 兜底 -->
      <el-input v-else v-model="model[f.name]" />
      <div v-if="f.hint" class="form-hint">{{ f.hint }}</div>
    </el-form-item>
  </template>
</template>
