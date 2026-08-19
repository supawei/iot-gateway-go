package smardaten

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"iot-gateway-go/internal/driver/byteorder"
	"iot-gateway-go/internal/model"
)

// ---------- 类型映射表 ----------

// controllerTypeToDriver 将平台 controller type 映射为网关驱动名。
// 仅覆盖本次重构要求的类型：1/2/3/21/23/24。
func controllerTypeToDriver(ct string) string {
	switch ct {
	case "1":
		return "modbus" // modbus-rtu
	case "2":
		return "modbus" // modbus-tcp
	case "3":
		return "opcua"
	case "21":
		return "modbus" // dlt645-2007（暂用 modbus 驱动承载，后续可替换为专用驱动）
	case "23":
		return "modbus" // modbus-rtu-over-tcp
	case "24":
		return "modbus" // dlt645-1997
	default:
		return ""
	}
}

// controllerTypeToMode 将平台 controller type 映射为网关 modbus mode。
func controllerTypeToMode(ct string) string {
	switch ct {
	case "1":
		return "rtu"
	case "2":
		return "tcp"
	case "23":
		return "rtu-over-tcp"
	default:
		return ""
	}
}

// comToSerialPort 将平台 com 编号映射为 Linux 串口设备路径。
func comToSerialPort(com int) string {
	switch com {
	case 1:
		return "/dev/ttyS2"
	case 2:
		return "/dev/ttyS3"
	case 3:
		return "/dev/ttyS4"
	case 4:
		return "/dev/ttyS5"
	default:
		return fmt.Sprintf("/dev/ttyS%d", com+1)
	}
}

// parityToString 将平台 parity 数字映射为网关字符串。
func parityToString(parity int) string {
	switch parity {
	case 0:
		return "N"
	case 1:
		return "O"
	case 2:
		return "E"
	default:
		return "N"
	}
}

// functionCodeToName 将平台 functionCode 数字映射为网关 pollBlocks function 名。
func functionCodeToName(fc int) string {
	switch fc {
	case 1:
		return "coil"
	case 2:
		return "discrete"
	case 3:
		return "holding"
	case 4:
		return "input"
	default:
		return "holding"
	}
}

// dataTypeToModel 将平台 dataType 枚举映射为 model.DataType。
func dataTypeToModel(dt int) model.DataType {
	switch dt {
	case 0:
		return model.DataTypeBool
	case 1:
		return model.DataTypeInt16
	case 2:
		return model.DataTypeInt32
	case 3:
		return model.DataTypeInt64
	case 4:
		return model.DataTypeFloat
	case 5:
		return model.DataTypeDouble
	case 6:
		return model.DataTypeString
	default:
		return model.DataTypeInt16
	}
}

// dataTypeWidth 返回 dataType 占用的寄存器数量（modbus 专用）。
func dataTypeWidth(dt int) int {
	switch dt {
	case 0: // BOOL → 1 register (coil)
		return 1
	case 1: // INT16 → 1 register
		return 1
	case 2: // INT32 → 2 registers
		return 2
	case 3: // INT64 → 4 registers
		return 4
	case 4: // FLOAT → 2 registers
		return 2
	case 5: // DOUBLE → 4 registers
		return 4
	default:
		return 1
	}
}

// ---------- 配置转换 ----------

// syncToGateway 将 application.json 中的控制器和设备同步到网关 SQLite。
// 以 controllerId/deviceId 为 key 做 upsert,不覆盖网关本地独有的配置。
// 同步前先与当前存储内容对比:**内容未变则跳过写入**(避免无谓的 SQLite 写与
// 调度器热加载通知)。只增改、不删除平台配置里仍然存在的实体;
// 对**平台已移除**的实体(曾由本插件同步、本次配置中已不存在)执行删除清理,
// 见 deleteOrphanSynced——仅清理平台同步创建的实体,不误删 Web UI 手工配置。
func (o *platformOutput) syncToGateway(cfg *ApplicationConfig) {
	if o.store == nil {
		return
	}

	// 先同步控制器 → Connection
	syncedControllers := make(map[string]bool)
	for _, ctrl := range cfg.Controllers {
		if ctrl.Specs.Enable != 1 {
			continue
		}
		conn, err := convertControllerToConnection(ctrl)
		if err != nil {
			slog.Warn("skip controller, convert failed", "controllerId", ctrl.ControllerID, "err", err)
			continue
		}
		// 打管理标记:本实体由平台同步创建/管理,孤儿清理据此区分手工配置。
		conn.ManagedBy = o.managedBy()
		if o.connectionNeedsSave(conn) {
			if err := o.store.SaveConnection(conn); err != nil {
				slog.Error("save connection failed", "controllerId", ctrl.ControllerID, "err", err)
				continue
			}
			slog.Info("synced controller", "id", conn.ID, "name", conn.Name, "driver", conn.Driver)
		} else {
			slog.Debug("controller unchanged, skip", "id", conn.ID)
		}
		syncedControllers[ctrl.ControllerID] = true
	}

	// 再同步设备 → Device
	for _, dev := range cfg.Devices {
		ctrl, ok := findController(cfg.Controllers, dev.ControllerID)
		if !ok {
			slog.Warn("skip device, controller not found", "deviceId", dev.DeviceID, "controllerId", dev.ControllerID)
			continue
		}
		if !syncedControllers[dev.ControllerID] {
			slog.Warn("skip device, controller not synced", "deviceId", dev.DeviceID, "controllerId", dev.ControllerID)
			continue
		}

		device, err := convertDevice(dev, ctrl)
		if err != nil {
			slog.Warn("skip device, convert failed", "deviceId", dev.DeviceID, "err", err)
			continue
		}
		// 打管理标记:本设备由平台同步创建/管理,孤儿清理据此区分手工配置。
		device.ManagedBy = o.managedBy()
		if o.deviceNeedsSave(device) {
			if err := o.store.SaveDevice(device); err != nil {
				slog.Error("save device failed", "deviceId", dev.DeviceID, "err", err)
				continue
			}
			slog.Info("synced device", "id", device.ID, "name", device.Name, "points", len(device.Points))
		} else {
			slog.Debug("device unchanged, skip", "id", device.ID)
		}
	}

	// 清理孤儿实体:删除此前由平台同步管理、但本次 application.json 中已不存在的
	// 连接与设备(平台为权威源;只删平台同步创建的实体,不误删 Web UI 手工配置)。
	o.deleteOrphanSynced(cfg)
}

// deleteOrphanSynced 删除此前由平台同步管理、但本次配置中已不存在的连接与设备。
//
// 管理集合不再落 settings 表:同步创建/更新的连接与设备在自身行上带 managed_by
// 标记(见 syncToGateway),孤儿集合 = 带本输出实例标记、但本次配置中已不存在的实体,
// 直接按行查询即可,无需任何 JSON 登记/写回。天然区分"平台同步创建"与
// "Web UI 手工配置"(后者 managed_by 为空)——只清理前者。
// 删除顺序:先设备后连接(连接被设备引用时删除会失败)。删除失败的孤儿仍带
// managed_by 标记,下次同步自然再次进入候选集合重试,无需额外持久化。
func (o *platformOutput) deleteOrphanSynced(cfg *ApplicationConfig) {
	if o.store == nil {
		return
	}
	manager := o.managedBy()

	// 本次配置中存在的 controller/device ID 集合(以此为"应保留"的权威集)。
	presentConns := make(map[string]bool)
	for _, ctrl := range cfg.Controllers {
		presentConns[ctrl.ControllerID] = true
	}
	presentDevs := make(map[string]bool)
	for _, dev := range cfg.Devices {
		presentDevs[dev.DeviceID] = true
	}

	// 先删设备:带标记但平台已移除的设备
	prevDevs, err := o.store.ListManagedDeviceIDs(manager)
	if err != nil {
		slog.Warn("list managed device ids failed, skip orphan cleanup", "manager", manager, "err", err)
		return
	}
	for _, id := range prevDevs {
		if presentDevs[id] {
			continue
		}
		if err := o.store.DeleteDevice(id); err != nil {
			slog.Warn("delete orphan device failed, kept tracked for retry", "deviceId", id, "err", err)
			continue
		}
		slog.Info("deleted orphan device (removed from platform config)", "deviceId", id)
	}

	// 再删连接:带标记但平台已移除的控制器
	prevConns, err := o.store.ListManagedConnectionIDs(manager)
	if err != nil {
		slog.Warn("list managed connection ids failed, skip orphan cleanup", "manager", manager, "err", err)
		return
	}
	for _, id := range prevConns {
		if presentConns[id] {
			continue
		}
		if err := o.store.DeleteConnection(id); err != nil {
			slog.Warn("delete orphan connection failed, kept tracked for retry", "connectionId", id, "err", err)
			continue
		}
		slog.Info("deleted orphan connection (removed from platform config)", "connectionId", id)
	}
}

// ---------- 孤儿清理:管理标记 ----------

// managedBy 返回本输出实例的平台管理标记,写入 connection/device 的 managed_by
// 列。按 output 实例隔离,避免多个 smardaten 输出实例互相清理对方创建的实体。
func (o *platformOutput) managedBy() string {
	return "smardaten:" + o.managedScope()
}

// managedScope 返回本输出实例的隔离键(outputID;直接构造时回退 default)。
func (o *platformOutput) managedScope() string {
	if o.outputID != "" {
		return o.outputID
	}
	return "default"
}

// connectionNeedsSave 目标连接当前不存在或内容不同才写入。
func (o *platformOutput) connectionNeedsSave(target model.Connection) bool {
	cur, err := o.store.GetConnection(target.ID)
	if err != nil {
		return true // 不存在或读取失败:保守写入
	}
	return !connectionEqual(cur, target)
}

// deviceNeedsSave 目标设备当前不存在或内容不同才写入。
func (o *platformOutput) deviceNeedsSave(target model.Device) bool {
	cur, err := o.store.GetDevice(target.ID)
	if err != nil {
		return true // 不存在或读取失败:保守写入
	}
	return !deviceEqual(cur, target)
}

// connectionEqual 判断存储中的连接与目标内容是否一致(含管理标记,便于
// 手工连接被平台覆盖后打上 managed_by,以及内容未变时跳过无谓写入)。
func connectionEqual(cur, target model.Connection) bool {
	return cur.Name == target.Name &&
		cur.Driver == target.Driver &&
		cur.ManagedBy == target.ManagedBy &&
		jsonEqual(cur.Config, target.Config)
}

// deviceEqual 判断存储中的设备与目标内容是否一致(含点位列表与管理标记)。
func deviceEqual(cur, target model.Device) bool {
	return cur.Name == target.Name &&
		cur.ConnectionID == target.ConnectionID &&
		cur.IntervalMs == target.IntervalMs &&
		cur.Enabled == target.Enabled &&
		cur.ManagedBy == target.ManagedBy &&
		jsonEqual(cur.Params, target.Params) &&
		pointsEqual(cur.Points, target.Points)
}

// pointsEqual 按序比较点位列表(Name/Address/DataType/Scale)。
func pointsEqual(a, b []model.Point) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Address != b[i].Address ||
			a[i].DataType != b[i].DataType || a[i].Scale != b[i].Scale {
			return false
		}
	}
	return true
}

// jsonEqual 语义比较两份 JSON(忽略键序/格式差异);任一侧非法时回退字节比较。
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv interface{}
	if err := json.Unmarshal(a, &av); err != nil {
		return bytes.Equal(a, b)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(av, bv)
}

// findController 在控制器列表中按 ID 查找。
func findController(controllers []PlatformController, id string) (PlatformController, bool) {
	for _, c := range controllers {
		if c.ControllerID == id {
			return c, true
		}
	}
	return PlatformController{}, false
}

// convertControllerToConnection 将平台控制器转换为网关 Connection。
func convertControllerToConnection(ctrl PlatformController) (model.Connection, error) {
	driverName := controllerTypeToDriver(ctrl.Type)
	if driverName == "" {
		return model.Connection{}, fmt.Errorf("unsupported controller type %q", ctrl.Type)
	}

	connConfig, err := convertControllerConfig(ctrl)
	if err != nil {
		return model.Connection{}, fmt.Errorf("convert config: %w", err)
	}

	configJSON, err := json.Marshal(connConfig)
	if err != nil {
		return model.Connection{}, fmt.Errorf("marshal config: %w", err)
	}

	return model.Connection{
		ID:     ctrl.ControllerID,
		Name:   ctrl.Specs.Name,
		Driver: driverName,
		Config: configJSON,
	}, nil
}

// convertControllerConfig 按 controller type 转换 configuration 为网关 Connection.Config。
func convertControllerConfig(ctrl PlatformController) (map[string]interface{}, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(ctrl.Specs.Configuration, &raw); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}

	switch ctrl.Type {
	case "1": // modbus-rtu
		return convertModbusRTUConfig(raw), nil
	case "2", "23": // modbus-tcp, modbus-rtu-over-tcp
		return convertModbusTCPConfig(raw, ctrl.Type), nil
	case "3": // opcua
		return convertOPCUAConfig(raw), nil
	case "21", "24": // dlt645
		return convertDLT645Config(raw), nil
	default:
		// 未知类型：原样透传
		return raw, nil
	}
}

// convertModbusRTUConfig 转换 modbus-rtu 配置。
func convertModbusRTUConfig(raw map[string]interface{}) map[string]interface{} {
	cfg := map[string]interface{}{
		"mode": "rtu",
	}

	if com, ok := getInt(raw, "com"); ok {
		cfg["serialPort"] = comToSerialPort(com)
	}
	if b, ok := getInt(raw, "baudRate"); ok {
		cfg["baudRate"] = b
	}
	if d, ok := getInt(raw, "dataBits"); ok {
		cfg["dataBits"] = d
	}
	if s, ok := getInt(raw, "stopBits"); ok {
		cfg["stopBits"] = s
	}
	if p, ok := getInt(raw, "parity"); ok {
		cfg["parity"] = parityToString(p)
	}
	if t, ok := getInt(raw, "timeOut"); ok {
		cfg["timeout"] = formatDuration(t)
	}

	return cfg
}

// convertModbusTCPConfig 转换 modbus-tcp / modbus-rtu-over-tcp 配置。
func convertModbusTCPConfig(raw map[string]interface{}, ctrlType string) map[string]interface{} {
	cfg := map[string]interface{}{
		"mode": controllerTypeToMode(ctrlType),
	}

	ip, _ := raw["ip"].(string)
	port, _ := getInt(raw, "port")
	if ip != "" && port > 0 {
		cfg["address"] = fmt.Sprintf("%s:%d", ip, port)
	} else if ip != "" {
		cfg["address"] = ip + ":502"
	}

	if t, ok := getInt(raw, "timeOut"); ok {
		cfg["timeout"] = formatDuration(t)
	}

	return cfg
}

// convertOPCUAConfig 转换 OPC UA 配置。
func convertOPCUAConfig(raw map[string]interface{}) map[string]interface{} {
	cfg := map[string]interface{}{
		"mode": "poll",
	}

	ip, _ := raw["ip"].(string)
	port, _ := getInt(raw, "port")
	if ip != "" && port > 0 {
		cfg["endpoint"] = fmt.Sprintf("opc.tcp://%s:%d", ip, port)
	} else if ip != "" {
		cfg["endpoint"] = fmt.Sprintf("opc.tcp://%s:4840", ip)
	}

	anonymous, _ := getInt(raw, "anonymous")
	if anonymous == 0 {
		if u, ok := raw["userName"].(string); ok && u != "" {
			cfg["username"] = u
		}
		if p, ok := raw["passwd"].(string); ok && p != "" {
			cfg["password"] = p
		}
	}

	cfg["securityMode"] = "none"

	if t, ok := getInt(raw, "timeOut"); ok {
		cfg["timeout"] = formatDuration(t)
	}

	return cfg
}

// convertDLT645Config 转换 DLT645 配置。
func convertDLT645Config(raw map[string]interface{}) map[string]interface{} {
	cfg := map[string]interface{}{
		"mode": "tcp",
	}

	ip, _ := raw["ip"].(string)
	port, _ := getInt(raw, "port")
	if ip != "" && port > 0 {
		cfg["address"] = fmt.Sprintf("%s:%d", ip, port)
	}

	if t, ok := getInt(raw, "timeOut"); ok {
		cfg["timeout"] = formatDuration(t)
	}

	return cfg
}

// convertDevice 将平台设备转换为网关 Device。
func convertDevice(dev PlatformDevice, ctrl PlatformController) (model.Device, error) {
	// 从 controller configuration 提取功能码（modbus 类型专用，用于地址前缀）
	functionName := modbusFunctionOf(ctrl)

	// 构建 Points：从 sensorList 中筛选该设备 properties 引用的点位
	pointIDs := make(map[string]bool)
	for _, prop := range dev.Properties {
		pointIDs[prop.PointID] = true
	}

	points := make([]model.Point, 0, len(pointIDs))
	for _, sensor := range ctrl.SensorList {
		if !pointIDs[sensor.PointID] {
			continue
		}
		point := model.Point{
			Name:     sensor.PointID,
			Address:  sensor.ItemName,
			DataType: dataTypeToModel(sensor.DataType),
			Scale:    scaleOf(sensor),
		}
		// modbus 类型：地址补功能码前缀（"2" → "holding:2"），
		// 网关 modbus 驱动的 parseAddress 严格要求 "function:register" 格式。
		if functionName != "" && !strings.Contains(point.Address, ":") {
			point.Address = functionName + ":" + point.Address
		}
		points = append(points, point)
	}

	// 构建 Device.Params：从 controller configuration 提取设备级参数
	params := convertDeviceParams(ctrl, points)

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return model.Device{}, fmt.Errorf("marshal params: %w", err)
	}

	// 采集周期
	intervalMs := ctrl.Specs.Period * 1000
	if intervalMs <= 0 {
		intervalMs = 5000 // 默认 5s
	}

	return model.Device{
		ID:           dev.DeviceID,
		Name:         dev.DeviceID, // 用 deviceId 作为名称
		ConnectionID: ctrl.ControllerID,
		Params:       paramsJSON,
		Points:       points,
		IntervalMs:   intervalMs,
		Enabled:      true,
	}, nil
}

// modbusFunctionOf 返回 controller 的 modbus 功能码名（holding/coil/input/discrete）；
// 非 modbus 类型返回空串（地址无需前缀）。
func modbusFunctionOf(ctrl PlatformController) string {
	switch ctrl.Type {
	case "1", "2", "23": // modbus-rtu / modbus-tcp / modbus-rtu-over-tcp
	default:
		return ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(ctrl.Specs.Configuration, &raw); err != nil {
		return ""
	}
	fc, ok := getInt(raw, "functionCode")
	if !ok {
		return ""
	}
	return functionCodeToName(fc)
}

// scaleOf 从 sensor 的 exDesc 提取缩放系数（caliMultiple）。
// 平台倍数为 0 或缺省时回退 1.0。
func scaleOf(sensor PlatformSensor) float64 {
	if len(sensor.ExDesc) == 0 {
		return 0
	}
	var ex struct {
		CaliMultiple float64 `json:"caliMultiple"`
	}
	if err := json.Unmarshal(sensor.ExDesc, &ex); err != nil {
		return 0
	}
	if ex.CaliMultiple == 0 {
		return 0
	}
	return ex.CaliMultiple
}

// byteOrderOfSensors 从控制器传感器列表推导设备级字节序。
// 平台按传感器（sensorList[].exDesc.regExInterByte/regExOuterOrder）配置字节序，
// 网关按设备级 Device.Params.byteOrder 承载。多个传感器配置一致时取该值；
// 不一致时取首个并告警（设备级承载无法表达同一设备内多种字节序，典型场景各传感器一致）。
func byteOrderOfSensors(ctrl PlatformController) (string, bool) {
	distinct := make(map[byteorder.Order]bool)
	var first byteorder.Order
	found := false
	for _, sensor := range ctrl.SensorList {
		if len(sensor.ExDesc) == 0 {
			continue
		}
		var ex struct {
			RegExInterByte  *int `json:"regExInterByte"`
			RegExOuterOrder *int `json:"regExOuterOrder"`
		}
		if err := json.Unmarshal(sensor.ExDesc, &ex); err != nil {
			continue
		}
		// 两个开关都未配置 → 该传感器无字节序配置,跳过
		if ex.RegExInterByte == nil && ex.RegExOuterOrder == nil {
			continue
		}
		interByte, outerOrder := 0, 0
		if ex.RegExInterByte != nil {
			interByte = *ex.RegExInterByte
		}
		if ex.RegExOuterOrder != nil {
			outerOrder = *ex.RegExOuterOrder
		}
		order := byteorder.FromSwaps(interByte, outerOrder)
		if !found {
			first = order
			found = true
		}
		distinct[order] = true
	}
	if !found {
		return "", false
	}
	if len(distinct) > 1 {
		orders := make([]string, 0, len(distinct))
		for o := range distinct {
			orders = append(orders, string(o))
		}
		slog.Warn("sensors have mixed byte orders, device byteOrder uses first sensor", "controllerId", ctrl.ControllerID, "orders", orders)
	}
	return string(first), true
}

// convertDeviceParams 从 controller configuration 提取设备级参数。
func convertDeviceParams(ctrl PlatformController, points []model.Point) map[string]interface{} {
	params := make(map[string]interface{})

	var raw map[string]interface{}
	if err := json.Unmarshal(ctrl.Specs.Configuration, &raw); err != nil {
		return params
	}

	// slaveId：从 controller 级移到 device 级
	if slaveID, ok := getInt(raw, "slaveId"); ok {
		params["slaveId"] = slaveID
	}

	// byteOrder：从传感器 exDesc 的字节序开关推导设备级字节序（modbus 专用）
	if order, ok := byteOrderOfSensors(ctrl); ok {
		params["byteOrder"] = order
	}

	// pollBlocks：从 functionCode + sensorList 计算
	if fc, ok := getInt(raw, "functionCode"); ok {
		params["pollBlocks"] = buildPollBlocks(fc, points)
	}

	return params
}

// buildPollBlocks 根据功能码和点位列表构建 pollBlocks。
// 将连续寄存器地址合并为一个 block，以支持批量读取。
func buildPollBlocks(functionCode int, points []model.Point) []map[string]interface{} {
	if len(points) == 0 {
		return nil
	}

	type addrRange struct {
		start int
		end   int
	}

	// 解析每个点位的地址和宽度，计算寄存器范围
	ranges := make([]addrRange, 0, len(points))
	for _, p := range points {
		addr, err := registerOf(p.Address)
		if err != nil {
			continue
		}
		width := dataTypeWidthForPoint(p.DataType)
		ranges = append(ranges, addrRange{start: addr, end: addr + width - 1})
	}

	if len(ranges) == 0 {
		return nil
	}

	// 按 start 排序
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })

	// 合并重叠或相邻（间隙 ≤16）的范围
	merged := []addrRange{ranges[0]}
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.start <= last.end+16 {
			if r.end > last.end {
				last.end = r.end
			}
		} else {
			merged = append(merged, r)
		}
	}

	blocks := make([]map[string]interface{}, 0, len(merged))
	for _, m := range merged {
		blocks = append(blocks, map[string]interface{}{
			"function": functionCodeToName(functionCode),
			"start":    m.start,
			"count":    m.end - m.start + 1,
		})
	}

	return blocks
}

// registerOf 从点位地址提取寄存器号，兼容 "holding:2" 和纯数字 "2" 两种格式。
func registerOf(addr string) (int, error) {
	// 带功能码前缀：取冒号后部分
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		addr = addr[i+1:]
	}
	return strconv.Atoi(strings.TrimSpace(addr))
}

// dataTypeWidthForPoint 根据 model.DataType 返回寄存器宽度。
func dataTypeWidthForPoint(dt model.DataType) int {
	switch dt {
	case model.DataTypeBool, model.DataTypeInt16, model.DataTypeUInt16:
		return 1
	case model.DataTypeInt32, model.DataTypeUInt32, model.DataTypeFloat:
		return 2
	case model.DataTypeInt64, model.DataTypeDouble:
		return 4
	default:
		return 1
	}
}

// ---------- 工具函数 ----------

// getInt 从 map 中获取 int 值（兼容 JSON 反序列化的 float64）。
func getInt(m map[string]interface{}, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(math.Round(n)), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// formatDuration 将毫秒数格式化为 Go duration 字符串。
func formatDuration(ms int) string {
	if ms <= 0 {
		return "1s"
	}
	d := float64(ms) / 1000.0
	// 去掉尾随零
	s := strconv.FormatFloat(d, 'f', -1, 64)
	return s + "s"
}
