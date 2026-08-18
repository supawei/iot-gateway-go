package sparkplugb

import (
	"bytes"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/output/mqtttest"
)

// fakeStore 实现 output.StoreAccessor(测试用)。
type fakeStore struct {
	devices map[string]model.Device
	order   []string
}

func (f *fakeStore) SaveConnection(model.Connection) error          { return nil }
func (f *fakeStore) SaveDevice(d model.Device) error                { f.devices[d.ID] = d; return nil }
func (f *fakeStore) GetConnection(string) (model.Connection, error) { return model.Connection{}, nil }
func (f *fakeStore) GetDevice(id string) (model.Device, error)      { return f.devices[id], nil }
func (f *fakeStore) ListDevices() ([]model.Device, error) {
	out := make([]model.Device, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.devices[id])
	}
	return out, nil
}
func (f *fakeStore) DeleteConnection(string) error                { return nil }
func (f *fakeStore) DeleteDevice(string) error                    { return nil }
func (f *fakeStore) GetSetting(string) (string, bool, error)      { return "", false, nil }
func (f *fakeStore) SetSetting(string, string) error              { return nil }

// fakeBackfill 记录被落库补传的点。
type fakeBackfill struct {
	saved []model.DataPoint
}

func (f *fakeBackfill) Save(_ string, dps []model.DataPoint) error {
	f.saved = append(f.saved, dps...)
	return nil
}

// RecordingBrokerWrapper 提供按 topic 过滤消息的便捷方法。
type RecordingBrokerWrapper struct{ *mqtttest.RecordingBroker }

// messagesByTopic 返回指定 topic 的消息。
func (w *RecordingBrokerWrapper) messagesByTopic(topic string) []mqtttest.RecordedMessage {
	var out []mqtttest.RecordedMessage
	for _, m := range w.Messages() {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}

// waitTopic 轮询直到某 topic 出现至少 n 条消息。
func (w *RecordingBrokerWrapper) waitTopic(t *testing.T, topic string, n int) []mqtttest.RecordedMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := w.messagesByTopic(topic); len(msgs) >= n {
			return msgs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("topic %q: got %d messages, want >= %d", topic, len(w.messagesByTopic(topic)), n)
	return nil
}

// ---- 最小 Sparkplug payload 解码器(仅测试用,校验 seq/metrics)----

type decodedMetric struct {
	name     string
	alias    uint64
	datatype uint32
}

// decodePayload 解析 payload 的 seq 与 metrics(只解析本实现用到的字段)。
func decodePayload(t *testing.T, b []byte) (seq uint32, metrics []decodedMetric) {
	t.Helper()
	pos := 0
	for pos < len(b) {
		tag, n := readVarint(b[pos:])
		pos += n
		field, wire := int(tag>>3), int(tag&7)
		switch {
		case field == 1 && wire == 0: // timestamp
			_, n = readVarint(b[pos:])
			pos += n
		case field == 2 && wire == 0: // seq
			v, n2 := readVarint(b[pos:])
			pos += n2
			seq = uint32(v)
		case field == 5 && wire == 2: // metric
			l, n2 := readVarint(b[pos:])
			pos += n2
			metrics = append(metrics, decodeMetric(t, b[pos:pos+int(l)]))
			pos += int(l)
		default:
			pos = skipField(t, b, pos, wire)
		}
	}
	return seq, metrics
}

func decodeMetric(t *testing.T, b []byte) decodedMetric {
	t.Helper()
	var m decodedMetric
	pos := 0
	for pos < len(b) {
		tag, n := readVarint(b[pos:])
		pos += n
		field, wire := int(tag>>3), int(tag&7)
		switch {
		case field == 1 && wire == 2: // name
			l, n2 := readVarint(b[pos:])
			pos += n2
			m.name = string(b[pos : pos+int(l)])
			pos += int(l)
		case field == 2 && wire == 0: // alias
			v, n2 := readVarint(b[pos:])
			pos += n2
			m.alias = v
		case field == 4 && wire == 0: // datatype
			v, n2 := readVarint(b[pos:])
			pos += n2
			m.datatype = uint32(v)
		default:
			pos = skipField(t, b, pos, wire)
		}
	}
	return m
}

func readVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b); i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v, len(b)
}

func skipField(t *testing.T, b []byte, pos, wire int) int {
	t.Helper()
	switch wire {
	case 0:
		_, n := readVarint(b[pos:])
		return pos + n
	case 1:
		return pos + 8
	case 5:
		return pos + 4
	case 2:
		l, n := readVarint(b[pos:])
		return pos + n + int(l)
	}
	t.Fatalf("unsupported wire type %d", wire)
	return pos
}

// ---- 测试 ----

// TestBirthOnConnectReal 用 RecordingBroker 验证出生消息的 topic 与 payload。
func TestBirthOnConnectReal(t *testing.T) {
	latest := map[string]interface{}{"temperature": 25.5, "running": true}
	b := mqtttest.StartRecording(t)
	st := &fakeStore{devices: map[string]model.Device{}}
	dev := model.Device{ID: "d1", Name: "d1", Enabled: true, Points: []model.Point{
		{Name: "temperature", Address: "holding:0", DataType: model.DataTypeDouble},
		{Name: "running", Address: "coil:0", DataType: model.DataTypeBool},
	}}
	st.devices["d1"] = dev
	st.order = []string{"d1"}

	bc := output.BuildContext{GatewayID: "gw-01", Store: st,
		LatestValues: func(string) map[string]interface{} { return latest }, OutputID: "spb-1"}
	o, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "spb", GroupID: "grp", EdgeNodeID: "en1"}, bc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer o.Close()
	w := &RecordingBrokerWrapper{b}

	// STATE ONLINE(retained)
	w.waitTopic(t, "spBv1.0/grp/STATE/en1", 1)
	if got := w.messagesByTopic("spBv1.0/grp/STATE/en1")[0].Payload; !bytes.Equal(got, []byte("ONLINE")) {
		t.Fatalf("STATE payload = %q, want ONLINE", got)
	}
	// NBIRTH(节点 topic)
	nbirth := w.waitTopic(t, "spBv1.0/grp/NBIRTH/en1", 1)[0]
	seq, metrics := decodePayload(t, nbirth.Payload)
	if seq == 0 {
		t.Fatal("NBIRTH seq should be non-zero")
	}
	if len(metrics) != 1 || metrics[0].name != "deviceCount" || metrics[0].datatype != DataTypeInt32 {
		t.Fatalf("NBIRTH metrics mismatch: %+v", metrics)
	}
	// DBIRTH(设备 topic,声明点位 name/alias/datatype)
	dbirth := w.waitTopic(t, "spBv1.0/grp/DBIRTH/en1/d1", 1)[0]
	_, mets := decodePayload(t, dbirth.Payload)
	if len(mets) != 2 {
		t.Fatalf("DBIRTH want 2 metrics, got %+v", mets)
	}
	byName := map[string]decodedMetric{}
	for _, m := range mets {
		byName[m.name] = m
	}
	temp := byName["temperature"]
	if temp.alias == 0 || temp.datatype != DataTypeDouble {
		t.Fatalf("temperature metric mismatch: %+v", temp)
	}
	if run := byName["running"]; run.alias == 0 || run.datatype != DataTypeBoolean {
		t.Fatalf("running metric mismatch: %+v", run)
	}
}

// TestPublishUsesAlias 设备出生后,Publish 发 DDATA 且用别名(不含点位名),值类型一致。
func TestPublishUsesAlias(t *testing.T) {
	b := mqtttest.StartRecording(t)
	st := &fakeStore{devices: map[string]model.Device{}}
	dev := model.Device{ID: "d1", Name: "d1", Enabled: true, Points: []model.Point{
		{Name: "temperature", Address: "holding:0", DataType: model.DataTypeDouble},
	}}
	st.devices["d1"] = dev
	st.order = []string{"d1"}

	bc := output.BuildContext{GatewayID: "gw-01", Store: st,
		LatestValues: func(string) map[string]interface{} { return map[string]interface{}{"temperature": 25.5} }, OutputID: "spb-1"}
	o, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "spb"}, bc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer o.Close()
	w := &RecordingBrokerWrapper{b}

	// 等出生完成,取 temperature 的别名(edge node 默认 = 网关 ID gw-01)。
	w.waitTopic(t, "spBv1.0/iot-gateway/DBIRTH/gw-01/d1", 1)
	spb := o.(*sparkplugOutput)
	spb.mu.Lock()
	alias := spb.meta["d1/temperature"].alias
	spb.mu.Unlock()
	if alias == 0 {
		t.Fatal("expected alias assigned at birth")
	}

	// Publish 一个点 → DDATA。
	if err := o.Publish(model.DataPoint{DeviceID: "d1", Point: "temperature", Value: 26.0, Timestamp: time.Now(), Quality: model.QualityGood}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	dd := w.waitTopic(t, "spBv1.0/iot-gateway/DDATA/gw-01/d1", 1)[0]
	if bytes.Contains(dd.Payload, []byte("temperature")) {
		t.Fatal("DDATA should use alias, not point name")
	}
	_, mets := decodePayload(t, dd.Payload)
	if len(mets) != 1 {
		t.Fatalf("DDATA want 1 metric, got %+v", mets)
	}
	if mets[0].alias != alias || mets[0].datatype != DataTypeDouble {
		t.Fatalf("DDATA metric mismatch: %+v (want alias %d double)", mets[0], alias)
	}
}

// TestDeviceLifecycle DeviceOnline → DBIRTH;DeviceOffline → DDEATH(空 payload)。
func TestDeviceLifecycle(t *testing.T) {
	b := mqtttest.StartRecording(t)
	st := &fakeStore{devices: map[string]model.Device{}}
	dev := model.Device{ID: "d1", Name: "d1", Enabled: true, Points: []model.Point{
		{Name: "p", Address: "a", DataType: model.DataTypeInt16},
	}}
	st.devices["d1"] = dev
	st.order = []string{"d1"}

	bc := output.BuildContext{GatewayID: "gw-01", Store: st, LatestValues: func(string) map[string]interface{} { return nil }, OutputID: "spb-1"}
	o, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "spb"}, bc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer o.Close()
	w := &RecordingBrokerWrapper{b}

	// 等节点出生完成(出生序列在 OnConnect 触发)。
	w.waitTopic(t, "spBv1.0/iot-gateway/DBIRTH/gw-01/d1", 1)
	nb, ok := o.(output.DeviceNotifier)
	if !ok {
		t.Fatal("output should implement DeviceNotifier")
	}

	// 运行时新增设备并上线 → DBIRTH。
	st.devices["d2"] = model.Device{ID: "d2", Name: "d2", Enabled: true, Points: []model.Point{
		{Name: "p", Address: "a", DataType: model.DataTypeInt16},
	}}
	nb.DeviceOnline("d2")
	w.waitTopic(t, "spBv1.0/iot-gateway/DBIRTH/gw-01/d2", 1)

	// 设备离线 → DDEATH(空 payload)。
	nb.DeviceOffline("d2")
	dd := w.waitTopic(t, "spBv1.0/iot-gateway/DDEATH/gw-01/d2", 1)[0]
	if len(dd.Payload) != 0 {
		t.Fatalf("DDEATH should have empty payload, got %d bytes", len(dd.Payload))
	}
}

// TestCloseSendsNDEATHAndStateOffline Close 发送 NDEATH(空)+ STATE OFFLINE。
func TestCloseSendsNDEATHAndStateOffline(t *testing.T) {
	b := mqtttest.StartRecording(t)
	st := &fakeStore{devices: map[string]model.Device{}}
	bc := output.BuildContext{GatewayID: "gw-01", Store: st, LatestValues: func(string) map[string]interface{} { return nil }, OutputID: "spb-1"}
	o, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "spb", GroupID: "g", EdgeNodeID: "e"}, bc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := &RecordingBrokerWrapper{b}
	w.waitTopic(t, "spBv1.0/g/STATE/e", 1)

	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ndeath := w.waitTopic(t, "spBv1.0/g/NDEATH/e", 1)[0]
	if len(ndeath.Payload) != 0 {
		t.Fatalf("NDEATH should have empty payload")
	}
	off := w.messagesByTopic("spBv1.0/g/STATE/e")
	if len(off) < 2 || !bytes.Equal(off[len(off)-1].Payload, []byte("OFFLINE")) {
		t.Fatalf("expected STATE OFFLINE after close, got %+v", off)
	}
}

// TestPublishBackfillWhenNotBorn 未出生(未连接)时 Publish 落库补传,不丢数据。
func TestPublishBackfillWhenNotBorn(t *testing.T) {
	bf := &fakeBackfill{}
	o := &sparkplugOutput{
		store:    &fakeStore{devices: map[string]model.Device{}},
		outputID: "spb-1",
		backfill: bf,
		born:     make(map[string]bool),
		meta:     make(map[string]metricMeta),
	}
	// 未出生(等价于未连接):数据应落库补传。
	if err := o.Publish(model.DataPoint{DeviceID: "d1", Point: "p", Value: 1.0, Timestamp: time.Now(), Quality: model.QualityGood}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(bf.saved) != 1 {
		t.Fatalf("want 1 point backfilled, got %d", len(bf.saved))
	}
	if o.BackfillHealthy() {
		t.Fatal("not born output should not be backfill healthy")
	}
}

// TestTopicSafe topic 保留字符被替换。
func TestTopicSafe(t *testing.T) {
	if got := topicSafe("a/b+c#d e"); got != "a_b_c_d_e" {
		t.Fatalf("topicSafe = %q", got)
	}
}

// TestSparkplugType 内部类型 → Sparkplug datatype 映射。
func TestSparkplugType(t *testing.T) {
	cases := []struct {
		dt  model.DataType
		spb uint32
	}{
		{model.DataTypeBool, DataTypeBoolean},
		{model.DataTypeInt16, DataTypeInt16},
		{model.DataTypeUInt16, DataTypeUInt16},
		{model.DataTypeInt32, DataTypeInt32},
		{model.DataTypeUInt32, DataTypeUInt32},
		{model.DataTypeInt64, DataTypeInt64},
		{model.DataTypeFloat, DataTypeFloat},
		{model.DataTypeDouble, DataTypeDouble},
		{model.DataTypeString, DataTypeString},
	}
	for _, tc := range cases {
		if got := sparkplugType(tc.dt); got != tc.spb {
			t.Fatalf("sparkplugType(%s) = %d, want %d", tc.dt, got, tc.spb)
		}
	}
}
