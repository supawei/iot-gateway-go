// Package sparkplugb 实现 Sparkplug B(工业 MQTT 事实标准)输出。
// 该文件是 SparkplugB.proto(Payload/Metric)的最小手写编码器:只编码不解析,
// 覆盖网关作为边缘节点所需的 birth/data/death 消息。字节编码遵循 protobuf wire
// format 与 Sparkplug B 规范(见 docs/sparkplugb.md)。
package sparkplugb

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Sparkplug B data type 枚举(与 SparkplugB.proto 的 DataType 一致)。
const (
	DataTypeUnknown          = 0
	DataTypeInt8             = 1
	DataTypeInt16            = 2
	DataTypeInt32            = 3
	DataTypeInt64            = 4
	DataTypeUInt8            = 5
	DataTypeUInt16           = 6
	DataTypeUInt32           = 7
	DataTypeUInt64           = 8
	DataTypeFloat            = 9
	DataTypeDouble           = 10
	DataTypeBoolean          = 11
	DataTypeString           = 12
	DataTypeDateTime         = 13
	DataTypeText             = 14
	DataTypeUUID             = 15
	DataTypeDataSet          = 16
	DataTypeBytes            = 17
	DataTypeFile             = 18
	DataTypeTemplate         = 19
	DataTypePropertySet      = 20
	DataTypePropertySetEntry = 21
)

// metric 是 Sparkplug Payload 里的一个 Metric(只含本网关用到的字段)。
type metric struct {
	name      string // field 1,仅 birth 声明时携带
	alias     uint64 // field 2,data 消息用别名替代 name
	timestamp uint64 // field 3,Unix 毫秒
	datatype  uint32 // field 4
	isNull    bool   // field 7
	// oneof value(field 8..13),按 datatype 决定写入哪个字段。
	value interface{}
}

// encodePayload 编码 Sparkplug Payload{timestamp, seq, metrics}。
// metrics 为空时返回仅含 timestamp+seq 的最小 payload。
func encodePayload(seq uint32, metrics []metric, now time.Time) []byte {
	b := appendTag(nil, 1, wireVarint) // timestamp uint64
	b = appendVarint(b, uint64(now.UnixMilli()))
	b = appendTag(b, 2, wireVarint) // seq uint32
	b = appendVarint(b, uint64(seq))
	for _, m := range metrics {
		mb := encodeMetric(m)
		if len(mb) == 0 {
			continue
		}
		b = appendTag(b, 5, wireBytes) // repeated Metric metrics
		b = appendVarint(b, uint64(len(mb)))
		b = append(b, mb...)
	}
	return b
}

// encodeMetric 编码单个 Metric。
func encodeMetric(m metric) []byte {
	var b []byte
	if m.name != "" {
		b = appendTag(b, 1, wireBytes)
		b = appendVarint(b, uint64(len(m.name)))
		b = append(b, m.name...)
	}
	if m.alias != 0 {
		b = appendTag(b, 2, wireVarint)
		b = appendVarint(b, m.alias)
	}
	if m.timestamp != 0 {
		b = appendTag(b, 3, wireVarint)
		b = appendVarint(b, m.timestamp)
	}
	if m.datatype != 0 {
		b = appendTag(b, 4, wireVarint)
		b = appendVarint(b, uint64(m.datatype))
	}
	if m.isNull {
		b = appendTag(b, 7, wireVarint)
		b = appendVarint(b, 1)
		return b // is_null 置位,不携带 value
	}
	if m.value == nil {
		return b
	}
	switch m.datatype {
	case DataTypeDouble: // field 8 double_value
		b = appendTag(b, 8, wireFixed64)
		b = appendFixed64(b, math.Float64bits(toFloat64(m.value)))
	case DataTypeFloat: // field 9 float_value
		b = appendTag(b, 9, wireFixed32)
		b = appendFixed32(b, math.Float32bits(float32(toFloat64(m.value))))
	case DataTypeBoolean: // field 12 boolean_value
		b = appendTag(b, 12, wireVarint)
		if toBool(m.value) {
			b = appendVarint(b, 1)
		} else {
			b = appendVarint(b, 0)
		}
	case DataTypeString, DataTypeText: // field 13 string_value
		s := toString(m.value)
		b = appendTag(b, 13, wireBytes)
		b = appendVarint(b, uint64(len(s)))
		b = append(b, s...)
	case DataTypeUInt64: // field 11 long_value(uint64)
		b = appendTag(b, 11, wireVarint)
		b = appendVarint(b, uint64(toInt64(m.value)))
	default:
		// int8/16/32/64,uint8/16/32 统一按 int64 编码(field 10,负数补码)。
		b = appendTag(b, 10, wireVarint)
		b = appendVarint(b, uint64(toInt64(m.value)))
	}
	return b
}

// toFloat64 把常见数值/布尔/JSON 数值统一转为 float64(供 double/float 编码)。
func toFloat64(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float32:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	}
	return 0
}

// toBool 把常见类型转为布尔(供 boolean 编码)。
func toBool(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x != 0
	case int8:
		return x != 0
	case int16:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case uint:
		return x != 0
	case uint8:
		return x != 0
	case uint16:
		return x != 0
	case uint32:
		return x != 0
	case uint64:
		return x != 0
	case float32:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return false
}

// toString 把值转为字符串(供 string/text 编码)。
func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// protobuf wire types。
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// appendTag 追加字段 tag:field<<3 | wire。
func appendTag(b []byte, field, wire int) []byte {
	return appendVarint(b, uint64(field<<3|wire))
}

// appendVarint 追加 base-128 varint。
func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// appendFixed64 追加小端 8 字节(fixed64/double)。
func appendFixed64(b []byte, v uint64) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	return append(b, buf[:]...)
}

// appendFixed32 追加小端 4 字节(fixed32/float)。
func appendFixed32(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}
