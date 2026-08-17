package smardaten

import (
	"encoding/json"
	"testing"

	"iot-gateway-go/internal/model"
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
      {"pointId": "p-temp", "itemName": "2", "dataType": 1, "exDesc": {"caliMultiple": 1.0}},
      {"pointId": "p-sw", "itemName": "1", "dataType": 1, "exDesc": {"caliMultiple": 1.0}}
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
	cases := []struct{ in string; want int }{
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
