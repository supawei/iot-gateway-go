package smardaten

import (
	"encoding/json"
	"strconv"
	"testing"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

// sampleApplicationJSON 是平台真实下发的 application.json 片段（脱敏），
// 用于验证自动同步转换（modbus-rtu-over-tcp, type=23）。
const sampleApplicationJSON = `{
  "devices": [{
    "deviceId": "dev-1",
    "controllerId": "ctrl-1",
    "properties": [
      {"identifier": "temperature", "pointId": "p-temp", "dataType": 1},
      {"identifier": "switch", "pointId": "p-sw", "dataType": 1}
    ]
  }],
  "controllers": [{
    "controllerId": "ctrl-1",
    "type": "23",
    "specs": {
      "cid": "ctrl-1",
      "enable": 1,
      "name": "空调控制器",
      "period": 10,
      "timerType": "0",
      "type": "23",
      "configuration": {"moduleType": "23", "functionCode": 3, "slaveId": 1, "port": 5020, "ip": "10.15.70.1", "timeOut": 1000}
    },
    "sensorList": [
      {"pointId": "p-temp", "itemName": "2", "dataType": 1, "exDesc": {"caliMultiple": 1.0, "regExInterByte": 0, "regExOuterOrder": 0}},
      {"pointId": "p-sw", "itemName": "1", "dataType": 1, "exDesc": {"caliMultiple": 1.0, "regExInterByte": 0, "regExOuterOrder": 0}}
    ]
  }]
}`

func loadSampleApp(t *testing.T) *ApplicationConfig {
	t.Helper()
	var cfg ApplicationConfig
	if err := json.Unmarshal([]byte(sampleApplicationJSON), &cfg); err != nil {
		t.Fatalf("parse sample application.json: %v", err)
	}
	return &cfg
}

// TestConvertDeviceAddressPrefix 验证 modbus 类型点位地址补功能码前缀。
// 平台 sensorList.itemName 是纯寄存器号（如 "2"），网关 modbus 驱动
// 的 parseAddress 要求 "function:register" 格式（如 "holding:2"）。
// 地址不带前缀会导致读取失败、设备被判离线。
func TestConvertDeviceAddressPrefix(t *testing.T) {
	cfg := loadSampleApp(t)
	dev := cfg.Devices[0]
	ctrl := cfg.Controllers[0]

	device, err := convertDevice(dev, ctrl)
	if err != nil {
		t.Fatalf("convertDevice: %v", err)
	}

	if len(device.Points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(device.Points))
	}

	// 地址必须带 holding: 前缀
	addrByName := map[string]string{}
	for _, p := range device.Points {
		addrByName[p.Name] = p.Address
	}
	if addrByName["p-temp"] != "holding:2" {
		t.Errorf("p-temp address = %q, want %q", addrByName["p-temp"], "holding:2")
	}
	if addrByName["p-sw"] != "holding:1" {
		t.Errorf("p-sw address = %q, want %q", addrByName["p-sw"], "holding:1")
	}

	// 缩放系数来自 exDesc.caliMultiple
	for _, p := range device.Points {
		if p.Scale != 1.0 {
			t.Errorf("%s scale = %v, want 1.0", p.Name, p.Scale)
		}
	}

	// slaveId 从 controller config 移到 Device.Params
	var params map[string]interface{}
	if err := json.Unmarshal(device.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["slaveId"] != float64(1) {
		t.Errorf("params.slaveId = %v, want 1", params["slaveId"])
	}

	// byteOrder 从传感器 exDesc 的 regExInterByte/regExOuterOrder 推导（0,0 → ABCD）
	if params["byteOrder"] != "ABCD" {
		t.Errorf("params.byteOrder = %v, want ABCD", params["byteOrder"])
	}

	// pollBlocks 应存在且 function=holding
	blocks, ok := params["pollBlocks"].([]interface{})
	if !ok || len(blocks) == 0 {
		t.Fatalf("params.pollBlocks missing or empty: %v", params["pollBlocks"])
	}
	first := blocks[0].(map[string]interface{})
	if first["function"] != "holding" {
		t.Errorf("pollBlocks[0].function = %v, want holding", first["function"])
	}

	// 采集周期: period=10s → 10000ms
	if device.IntervalMs != 10000 {
		t.Errorf("IntervalMs = %d, want 10000", device.IntervalMs)
	}
}

// TestConvertConnectionType23 验证 type=23 (modbus-rtu-over-tcp) 连接转换。
func TestConvertConnectionType23(t *testing.T) {
	cfg := loadSampleApp(t)
	ctrl := cfg.Controllers[0]

	conn, err := convertControllerToConnection(ctrl)
	if err != nil {
		t.Fatalf("convertControllerToConnection: %v", err)
	}

	if conn.Driver != "modbus" {
		t.Errorf("driver = %q, want modbus", conn.Driver)
	}
	if conn.ID != "ctrl-1" {
		t.Errorf("id = %q, want ctrl-1", conn.ID)
	}

	var ccfg map[string]interface{}
	if err := json.Unmarshal(conn.Config, &ccfg); err != nil {
		t.Fatalf("unmarshal connection config: %v", err)
	}
	if ccfg["mode"] != "rtu-over-tcp" {
		t.Errorf("mode = %v, want rtu-over-tcp", ccfg["mode"])
	}
	if ccfg["address"] != "10.15.70.1:5020" {
		t.Errorf("address = %v, want 10.15.70.1:5020", ccfg["address"])
	}
	if ccfg["timeout"] != "1s" {
		t.Errorf("timeout = %v, want 1s", ccfg["timeout"])
	}
}

// TestRegisterOf 验证寄存器号提取兼容两种地址格式。
func TestRegisterOf(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"holding:2", 2},
		{"coil:15", 15},
		{"2", 2},
		{" 3 ", 3},
	}
	for _, tc := range cases {
		got, err := registerOf(tc.in)
		if err != nil {
			t.Errorf("registerOf(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("registerOf(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestConvertDeviceNonModbus 验证非 modbus 类型（如 opcua）地址不加前缀。
func TestConvertDeviceNonModbus(t *testing.T) {
	var ctrl PlatformController
	ctrl.ControllerID = "opc-1"
	ctrl.Type = "3"
	ctrl.Specs = PlatformControllerSpecs{
		Enable:        1,
		Name:          "OPC",
		Period:        5,
		Configuration: json.RawMessage(`{"ip":"1.2.3.4","port":4840}`),
	}
	ctrl.SensorList = []PlatformSensor{
		{PointID: "n1", ItemName: "ns=2;i=5", DataType: 5},
	}

	dev := PlatformDevice{
		DeviceID:     "d-opc",
		ControllerID: "opc-1",
		Properties:   []PlatformProperty{{Identifier: "temp", PointID: "n1", DataType: 5}},
	}

	device, err := convertDevice(dev, ctrl)
	if err != nil {
		t.Fatalf("convertDevice: %v", err)
	}
	if len(device.Points) != 1 {
		t.Fatalf("expected 1 point, got %d", len(device.Points))
	}
	// OPC UA 地址（node id）保持原样，不加 modbus 前缀
	if device.Points[0].Address != "ns=2;i=5" {
		t.Errorf("opcua address = %q, want ns=2;i=5", device.Points[0].Address)
	}
	if device.Points[0].DataType != model.DataTypeDouble {
		t.Errorf("dataType = %q, want float64", device.Points[0].DataType)
	}
}

// TestConvertDeviceByteOrder 验证平台传感器 exDesc 的字节序开关
// （regExInterByte/regExOuterOrder）映射为网关 Device.Params.byteOrder。
func TestConvertDeviceByteOrder(t *testing.T) {
	tests := []struct {
		name       string
		interByte  int
		outerOrder int
		want       string
	}{
		{"big endian", 0, 0, "ABCD"},
		{"byte swap", 1, 0, "BADC"},
		{"word swap", 0, 1, "CDAB"},
		{"byte+word swap", 1, 1, "DCBA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := PlatformController{
				ControllerID: "c1",
				Type:         "2", // modbus-tcp
				Specs: PlatformControllerSpecs{
					Enable:        1,
					Period:        10,
					Configuration: json.RawMessage(`{"moduleType":"2","functionCode":3,"slaveId":1}`),
				},
				SensorList: []PlatformSensor{
					{PointID: "p1", ItemName: "0", DataType: 4, // float32 占 2 寄存器
						ExDesc: json.RawMessage(`{"caliMultiple":1.0,"regExInterByte":` + itoa(tc.interByte) + `,"regExOuterOrder":` + itoa(tc.outerOrder) + `}`)},
				},
			}
			dev := PlatformDevice{
				DeviceID:     "d1",
				ControllerID: "c1",
				Properties:   []PlatformProperty{{Identifier: "v", PointID: "p1", DataType: 4}},
			}
			device, err := convertDevice(dev, ctrl)
			if err != nil {
				t.Fatalf("convertDevice: %v", err)
			}
			var params map[string]interface{}
			if err := json.Unmarshal(device.Params, &params); err != nil {
				t.Fatalf("unmarshal params: %v", err)
			}
			if params["byteOrder"] != tc.want {
				t.Errorf("byteOrder = %v, want %s", params["byteOrder"], tc.want)
			}
		})
	}
}

// TestConvertDeviceByteOrderMixed 验证同一控制器内传感器字节序不一致时取首个并告警。
func TestConvertDeviceByteOrderMixed(t *testing.T) {
	ctrl := PlatformController{
		ControllerID: "c1",
		Type:         "2",
		Specs: PlatformControllerSpecs{
			Enable:        1,
			Configuration: json.RawMessage(`{"moduleType":"2","functionCode":3,"slaveId":1}`),
		},
		SensorList: []PlatformSensor{
			{PointID: "p1", ItemName: "0", DataType: 4, ExDesc: json.RawMessage(`{"regExInterByte":1,"regExOuterOrder":0}`)}, // BADC
			{PointID: "p2", ItemName: "2", DataType: 4, ExDesc: json.RawMessage(`{"regExInterByte":0,"regExOuterOrder":1}`)}, // CDAB
		},
	}
	dev := PlatformDevice{
		DeviceID:     "d1",
		ControllerID: "c1",
		Properties: []PlatformProperty{
			{Identifier: "v1", PointID: "p1", DataType: 4},
			{Identifier: "v2", PointID: "p2", DataType: 4},
		},
	}
	device, err := convertDevice(dev, ctrl)
	if err != nil {
		t.Fatalf("convertDevice: %v", err)
	}
	var params map[string]interface{}
	if err := json.Unmarshal(device.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["byteOrder"] != "BADC" {
		t.Errorf("byteOrder = %v, want BADC (first sensor)", params["byteOrder"])
	}
}

// TestConvertDeviceByteOrderMissing 验证传感器无字节序配置时不写 byteOrder（网关默认 ABCD）。
func TestConvertDeviceByteOrderMissing(t *testing.T) {
	ctrl := PlatformController{
		ControllerID: "c1",
		Type:         "2",
		Specs: PlatformControllerSpecs{
			Enable:        1,
			Configuration: json.RawMessage(`{"moduleType":"2","functionCode":3,"slaveId":1}`),
		},
		SensorList: []PlatformSensor{
			{PointID: "p1", ItemName: "0", DataType: 1, ExDesc: json.RawMessage(`{"caliMultiple":1.0}`)},
		},
	}
	dev := PlatformDevice{
		DeviceID:     "d1",
		ControllerID: "c1",
		Properties:   []PlatformProperty{{Identifier: "v", PointID: "p1", DataType: 1}},
	}
	device, err := convertDevice(dev, ctrl)
	if err != nil {
		t.Fatalf("convertDevice: %v", err)
	}
	var params map[string]interface{}
	if err := json.Unmarshal(device.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if _, ok := params["byteOrder"]; ok {
		t.Errorf("byteOrder should be absent when sensors have no byte order config, got %v", params["byteOrder"])
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// ---- 同步增量跳过(内容对比,无变化不写) ----

// sampleSyncConfig 构造一个典型的 modbus-tcp 控制器 + 设备同步配置。
func sampleSyncConfig() *ApplicationConfig {
	return &ApplicationConfig{
		Controllers: []PlatformController{{
			ControllerID: "ctrl-1",
			Type:         "2", // modbus-tcp
			Specs: PlatformControllerSpecs{
				Enable: 1,
				Name:   "ctrl-1",
				Period: 2,
				Configuration: json.RawMessage(
					`{"ip":"192.168.1.10","port":502,"slaveId":1,"functionCode":3}`),
			},
			SensorList: []PlatformSensor{
				{PointID: "p1", ItemName: "0", DataType: 2}, // INT32
				{PointID: "p2", ItemName: "1", DataType: 4}, // FLOAT
			},
		}},
		Devices: []PlatformDevice{{
			DeviceID:     "dev-1",
			ControllerID: "ctrl-1",
			Properties: []PlatformProperty{
				{Identifier: "temp", PointID: "p1"},
				{Identifier: "volt", PointID: "p2"},
			},
		}},
	}
}

// TestSyncToGatewayIdempotent 相同配置二次同步应跳过写入(内容一致,不产生热加载通知)。
func TestSyncToGatewayIdempotent(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	o := &platformOutput{store: st}

	cfg := sampleSyncConfig()
	o.syncToGateway(cfg)

	conn, err := st.GetConnection("ctrl-1")
	if err != nil || conn.ID != "ctrl-1" {
		t.Fatalf("connection not synced: %v", err)
	}
	dev, err := st.GetDevice("dev-1")
	if err != nil || len(dev.Points) != 2 {
		t.Fatalf("device not synced: %v, points=%d", err, len(dev.Points))
	}

	// 已落库内容视为"目标":内容一致 → 不需要保存。
	if o.connectionNeedsSave(conn) {
		t.Fatal("identical connection should not need save")
	}
	if o.deviceNeedsSave(dev) {
		t.Fatal("identical device should not need save")
	}

	// 再次同步:内容应保持不变(幂等)。
	o.syncToGateway(cfg)
	dev2, err := st.GetDevice("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if !deviceEqual(dev2, dev) {
		t.Fatal("idempotent sync changed device content")
	}
}

// TestSyncNeedsSaveOnChange 内容变化时 needsSave 返回 true。
func TestSyncNeedsSaveOnChange(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()
	o := &platformOutput{store: st}

	cfg := sampleSyncConfig()
	o.syncToGateway(cfg)
	dev, _ := st.GetDevice("dev-1")

	if !o.deviceNeedsSave(model.Device{ID: "nonexistent"}) {
		t.Fatal("absent device should need save")
	}

	// 点位变化 → true
	changed := dev
	changed.Points = append([]model.Point(nil), dev.Points...)
	changed.Points[0] = model.Point{Name: "p1", Address: "holding:100", DataType: model.DataTypeInt32}
	if !o.deviceNeedsSave(changed) {
		t.Fatal("point change should need save")
	}

	// 参数变化 → true
	changed = dev
	changed.Params = []byte(`{"slaveId":9}`)
	if !o.deviceNeedsSave(changed) {
		t.Fatal("param change should need save")
	}

	// 间隔变化 → true
	changed = dev
	changed.IntervalMs = 9999
	if !o.deviceNeedsSave(changed) {
		t.Fatal("interval change should need save")
	}

	// 连接配置变化 → true
	conn, _ := st.GetConnection("ctrl-1")
	if !o.connectionNeedsSave(model.Connection{ID: "ctrl-1"}) {
		t.Fatal("absent connection should need save")
	}
	connChanged := conn
	connChanged.Config = []byte(`{"ip":"10.0.0.1"}`)
	if !o.connectionNeedsSave(connChanged) {
		t.Fatal("connection config change should need save")
	}
}

// TestJsonEqual JSON 语义比较忽略键序/格式差异。
func TestJsonEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`, true}, // 键序不同
		{`{"a": 1}`, `{"a":1}`, true},            // 空格不同
		{`{"a":1}`, `{"a":2}`, false},            // 值不同
		{`{"a":[1,2]}`, `{"a":[2,1]}`, false},    // 数组顺序敏感
		{`{"a":"x"}`, `{"a":"x"}`, true},
		{`not-json`, `not-json`, true}, // 非法 JSON 回退字节比较
		{`not-json`, `other`, false},
	}
	for i, c := range cases {
		if got := jsonEqual(json.RawMessage(c.a), json.RawMessage(c.b)); got != c.want {
			t.Fatalf("case %d: jsonEqual(%s, %s) = %v, want %v", i, c.a, c.b, got, c.want)
		}
	}
}

// ---- 孤儿清理(平台已移除的实体删除) ----

// syncConfigWith 构造含指定控制器的同步配置(每个控制器一个设备,一一对应)。
func syncConfigWith(ctrlIDs []string) *ApplicationConfig {
	cfg := &ApplicationConfig{}
	for _, cid := range ctrlIDs {
		cfg.Controllers = append(cfg.Controllers, PlatformController{
			ControllerID: cid,
			Type:         "2", // modbus-tcp
			Specs: PlatformControllerSpecs{
				Enable: 1,
				Name:   cid,
				Period: 2,
				Configuration: json.RawMessage(
					`{"ip":"192.168.1.10","port":502,"slaveId":1,"functionCode":3}`),
			},
			SensorList: []PlatformSensor{
				{PointID: "p1", ItemName: "0", DataType: 2},
			},
		})
		cfg.Devices = append(cfg.Devices, PlatformDevice{
			DeviceID:     "dev-" + cid,
			ControllerID: cid,
			Properties:   []PlatformProperty{{Identifier: "temp", PointID: "p1"}},
		})
	}
	return cfg
}

// TestSyncDeletesOrphanEntities 平台移除控制器/设备后,同步应删除曾同步过的对应实体。
func TestSyncDeletesOrphanEntities(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	o := &platformOutput{store: st, outputID: "out-1"}

	// 首次同步:c1、c2 及各自设备
	o.syncToGateway(syncConfigWith([]string{"c1", "c2"}))
	if _, err := st.GetConnection("c1"); err != nil {
		t.Fatalf("c1 not synced: %v", err)
	}
	if _, err := st.GetDevice("dev-c2"); err != nil {
		t.Fatalf("dev-c2 not synced: %v", err)
	}

	// 平台移除 c2(及其设备)
	o.syncToGateway(syncConfigWith([]string{"c1"}))

	// c2 与其设备应被删除
	if _, err := st.GetConnection("c2"); err == nil {
		t.Fatal("orphan connection c2 should be deleted")
	}
	if _, err := st.GetDevice("dev-c2"); err == nil {
		t.Fatal("orphan device dev-c2 should be deleted")
	}
	// c1 保留
	if _, err := st.GetConnection("c1"); err != nil {
		t.Fatalf("c1 should remain: %v", err)
	}
	if _, err := st.GetDevice("dev-c1"); err != nil {
		t.Fatalf("dev-c1 should remain: %v", err)
	}
}

// TestSyncKeepsManualEntities 孤儿清理只删"平台同步创建"的实体,不误删 Web UI 手工配置。
func TestSyncKeepsManualEntities(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()
	o := &platformOutput{store: st, outputID: "out-1"}

	o.syncToGateway(syncConfigWith([]string{"c1"}))

	// 手工创建连接与设备(不经平台同步,不在管理集合内)
	manualConn := model.Connection{ID: "manual-conn", Name: "manual", Driver: "modbus", Config: json.RawMessage(`{"mode":"tcp"}`)}
	if err := st.SaveConnection(manualConn); err != nil {
		t.Fatal(err)
	}
	manualDev := model.Device{ID: "manual-dev", Name: "manual-dev", ConnectionID: "manual-conn", Params: json.RawMessage(`{}`), IntervalMs: 5000, Enabled: true}
	if err := st.SaveDevice(manualDev); err != nil {
		t.Fatal(err)
	}

	// 平台移除 c1
	o.syncToGateway(syncConfigWith(nil))

	if _, err := st.GetConnection("c1"); err == nil {
		t.Fatal("orphan c1 should be deleted")
	}
	if _, err := st.GetConnection("manual-conn"); err != nil {
		t.Fatalf("manual connection should remain: %v", err)
	}
	if _, err := st.GetDevice("manual-dev"); err != nil {
		t.Fatalf("manual device should remain: %v", err)
	}
}

// TestSyncOrphanPersistentAcrossRestart 同步创建实体带 managed_by 标记(存行上),
// 新实例(同 outputID,模拟重启)仍能据此清理孤儿。
func TestSyncOrphanPersistentAcrossRestart(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()

	// 第一"进程"实例
	o1 := &platformOutput{store: st, outputID: "out-1"}
	o1.syncToGateway(syncConfigWith([]string{"c1"}))
	if _, err := st.GetConnection("c1"); err != nil {
		t.Fatalf("c1 not synced: %v", err)
	}

	// 第二"进程"实例(同 outputID,按行上标记识别管理实体)
	o2 := &platformOutput{store: st, outputID: "out-1"}
	o2.syncToGateway(syncConfigWith(nil))

	if _, err := st.GetConnection("c1"); err == nil {
		t.Fatal("orphan c1 should be deleted after restart-like sync")
	}
}

// TestSyncOrphanPerInstanceIsolation 不同 output 实例的管理标记互不干扰。
func TestSyncOrphanPerInstanceIsolation(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()

	oA := &platformOutput{store: st, outputID: "out-A"}
	oA.syncToGateway(syncConfigWith([]string{"c1"}))

	// B 实例配置里没有 c1,但不应删除 A 同步创建的 c1
	oB := &platformOutput{store: st, outputID: "out-B"}
	oB.syncToGateway(syncConfigWith([]string{"cB"}))
	if _, err := st.GetConnection("c1"); err != nil {
		t.Fatalf("out-B should not delete out-A's c1: %v", err)
	}
	if _, err := st.GetConnection("cB"); err != nil {
		t.Fatalf("cB not synced: %v", err)
	}

	// A 实例随后移除 cA2,应由 A 自己清理
	oA.syncToGateway(syncConfigWith([]string{"c1", "cA2"}))
	oA.syncToGateway(syncConfigWith([]string{"c1"}))
	if _, err := st.GetConnection("cA2"); err == nil {
		t.Fatal("out-A orphan cA2 should be deleted by out-A")
	}
}

// TestSyncOrphanConnectionInUseRetained 连接仍被手工设备引用时删除失败,
// 连接行保留 managed_by 标记,引用解除后下次同步重试清理。
func TestSyncOrphanConnectionInUseRetained(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()
	o := &platformOutput{store: st, outputID: "out-1"}

	o.syncToGateway(syncConfigWith([]string{"c1"}))

	// 手工设备仍引用 c1(连接被占用,删除应失败)
	manualDev := model.Device{ID: "manual-dev", Name: "manual-dev", ConnectionID: "c1", Params: json.RawMessage(`{}`), IntervalMs: 5000, Enabled: true}
	if err := st.SaveDevice(manualDev); err != nil {
		t.Fatal(err)
	}

	o.syncToGateway(syncConfigWith(nil))

	// 平台设备 dev-c1 被删;c1 因被引用而保留
	if _, err := st.GetDevice("dev-c1"); err == nil {
		t.Fatal("dev-c1 should be deleted")
	}
	if _, err := st.GetConnection("c1"); err != nil {
		t.Fatalf("in-use connection c1 should be retained: %v", err)
	}

	// 手工设备删除后,下次同步应清理 c1(连接行仍带标记,可重试)
	if err := st.DeleteDevice("manual-dev"); err != nil {
		t.Fatal(err)
	}
	o.syncToGateway(syncConfigWith(nil))
	if _, err := st.GetConnection("c1"); err == nil {
		t.Fatal("c1 should be deleted once no longer in use")
	}
}

// TestSyncMarksManagedBy 同步创建的连接/设备在自身行上打 managed_by 标记
// (方案C,不再落 settings JSON),手工配置的实体不带标记;孤儿清理据此区分。
func TestSyncMarksManagedBy(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()
	o := &platformOutput{store: st, outputID: "out-1"}

	o.syncToGateway(syncConfigWith([]string{"c1"}))

	conn, err := st.GetConnection("c1")
	if err != nil {
		t.Fatalf("get c1: %v", err)
	}
	if conn.ManagedBy != "smardaten:out-1" {
		t.Errorf("synced connection ManagedBy = %q, want %q", conn.ManagedBy, "smardaten:out-1")
	}
	dev, err := st.GetDevice("dev-c1")
	if err != nil {
		t.Fatalf("get dev-c1: %v", err)
	}
	if dev.ManagedBy != "smardaten:out-1" {
		t.Errorf("synced device ManagedBy = %q, want %q", dev.ManagedBy, "smardaten:out-1")
	}

	// 手工配置的实体不带标记
	manualConn := model.Connection{ID: "manual-conn", Name: "manual", Driver: "modbus", Config: json.RawMessage(`{"mode":"tcp"}`)}
	if err := st.SaveConnection(manualConn); err != nil {
		t.Fatal(err)
	}
	manualDev := model.Device{ID: "manual-dev", Name: "manual-dev", ConnectionID: "manual-conn", Params: json.RawMessage(`{}`), IntervalMs: 5000, Enabled: true}
	if err := st.SaveDevice(manualDev); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.GetConnection("manual-conn"); c.ManagedBy != "" {
		t.Errorf("manual connection ManagedBy = %q, want empty", c.ManagedBy)
	}
	if d, _ := st.GetDevice("manual-dev"); d.ManagedBy != "" {
		t.Errorf("manual device ManagedBy = %q, want empty", d.ManagedBy)
	}

	// ListManaged* 只含本实例同步创建、且按实例隔离
	connIDs, err := st.ListManagedConnectionIDs("smardaten:out-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(connIDs) != 1 || connIDs[0] != "c1" {
		t.Errorf("ListManagedConnectionIDs = %v, want [c1]", connIDs)
	}
	devIDs, err := st.ListManagedDeviceIDs("smardaten:out-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(devIDs) != 1 || devIDs[0] != "dev-c1" {
		t.Errorf("ListManagedDeviceIDs = %v, want [dev-c1]", devIDs)
	}
	if ids, _ := st.ListManagedConnectionIDs("smardaten:out-other"); len(ids) != 0 {
		t.Errorf("other instance saw managed conns %v, want empty", ids)
	}
}

// TestSyncOrphanNoSettingsRegistry 方案C下孤儿清理不再写 settings 的 JSON 登记:
// 同步/清理后 settings 表不应存在 smardaten.sync.managed.* 键,孤儿清理完全
// 依赖实体行的 managed_by 标记。
func TestSyncOrphanNoSettingsRegistry(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()
	o := &platformOutput{store: st, outputID: "out-1"}

	o.syncToGateway(syncConfigWith([]string{"c1", "c2"}))
	o.syncToGateway(syncConfigWith([]string{"c1"})) // 平台移除 c2

	if _, ok, _ := st.GetSetting("smardaten.sync.managed.connections.out-1"); ok {
		t.Error("settings still has legacy managed-connections key")
	}
	if _, ok, _ := st.GetSetting("smardaten.sync.managed.devices.out-1"); ok {
		t.Error("settings still has legacy managed-devices key")
	}
	if _, err := st.GetConnection("c2"); err == nil {
		t.Error("orphan c2 should still be deleted via managed_by marker")
	}
	if _, err := st.GetConnection("c1"); err != nil {
		t.Errorf("c1 should remain: %v", err)
	}
}
