package output

// 本文件声明北向输出配置字段的 schema 类型,供输出插件声明自身配置结构,
// Web UI 据此动态渲染表单(与 driver.SchemaProvider 同一思路,实现"新增输出插件零前端改动")。

// FieldType 描述配置字段对应的输入控件类型。
type FieldType string

const (
	FieldString   FieldType = "string"   // 文本
	FieldPassword FieldType = "password" // 密码/令牌(前端以密码框渲染,不回显)
	FieldInt      FieldType = "int"      // 整数
	FieldNumber   FieldType = "number"   // 数值(可小数)
	FieldBool     FieldType = "bool"     // 开关
	FieldEnum     FieldType = "enum"     // 下拉选择(配合 Options)
	FieldJSON     FieldType = "json"     // 复杂嵌套结构,用 JSON 编辑
)

// ShowWhen 描述字段的显示条件:当依赖字段(Field)的值属于 In 集合时显示该字段。
type ShowWhen struct {
	Field string   `json:"field"`
	In    []string `json:"in"`
}

// Field 描述一个配置字段。JSON 结构与 driver.Field 保持一致,前端 SchemaForm 可复用。
type Field struct {
	Name        string    `json:"name"`                  // JSON key
	Label       string    `json:"label"`                 // 展示名
	Type        FieldType `json:"type"`                  // 控件类型
	Required    bool      `json:"required,omitempty"`    // 是否必填
	Default     any       `json:"default,omitempty"`     // 默认值
	Options     []string  `json:"options,omitempty"`     // enum 的可选项
	Hint        string    `json:"hint,omitempty"`        // 补充说明
	Placeholder string    `json:"placeholder,omitempty"` // 占位提示
	ShowWhen    *ShowWhen `json:"showWhen,omitempty"`    // 显示条件
}
