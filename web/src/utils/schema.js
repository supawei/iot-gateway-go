// 驱动配置 schema 的表单模型转换工具。
// 表单模型:每个字段名对应一个 key;json 字段存 JSON 字符串(编辑用),其余存原生值。

// defaultModel 按 schema 生成带默认值的表单模型。
export function defaultModel(schema) {
  const m = {}
  for (const f of schema || []) {
    if (f.default !== undefined) {
      m[f.name] = f.type === 'json' ? JSON.stringify(f.default, null, 2) : f.default
    } else if (f.type === 'json') {
      m[f.name] = ''
    } else if (f.type === 'bool') {
      m[f.name] = false
    } else if (f.type === 'int' || f.type === 'number') {
      m[f.name] = 0
    } else {
      m[f.name] = ''
    }
  }
  return m
}

// modelFromValue 把已保存的 config/params 对象转成表单模型。
export function modelFromValue(schema, value) {
  const m = defaultModel(schema)
  const src = value || {}
  for (const f of schema || []) {
    if (f.name in src) {
      m[f.name] = f.type === 'json' ? JSON.stringify(src[f.name], null, 2) : src[f.name]
    }
  }
  return m
}

// valueFromModel 把表单模型转成 config/params 对象;json 字段解析,非法则抛错。
export function valueFromModel(schema, model) {
  const out = {}
  for (const f of schema || []) {
    let v = model[f.name]
    if (v === '' || v === undefined || v === null) {
      if (f.required) {
        throw new Error(`「${f.label}」为必填项`)
      }
      continue
    }
    if (f.type === 'json') {
      try {
        out[f.name] = JSON.parse(v)
      } catch {
        throw new Error(`「${f.label}」不是合法 JSON`)
      }
    } else if (f.type === 'int') {
      out[f.name] = Math.trunc(Number(v))
    } else if (f.type === 'number') {
      out[f.name] = Number(v)
    } else {
      out[f.name] = v
    }
  }
  return out
}
