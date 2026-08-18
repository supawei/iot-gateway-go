# 监听类协议(Listener)设计

## 背景

网关的南向接入按"谁发起连接"分两类:

| 类型 | 谁发起连接 | 数据怎么来 | 现有能力接口 |
|---|---|---|---|
| 轮询(poll) | 网关主动连设备 | 网关问、设备答 | `Conn.Read` |
| 订阅(subscribe) | 网关主动连设备,再订阅 | 设备推 | `Subscriber` |
| **监听(listen)** | **设备主动连网关** | 设备推 | **`Listener`(本设计)** |

前两类网关是 TCP 客户端;监听类网关是 TCP 服务端——设备(或上游主站)主动连入并上报数据。典型场景:Modbus 设备按周期把数据"推"到网关监听端口(参考工程 `iot-gateway/iot_gateway/plugins/modbus_listen`,其 `eTimingType = ALARM_TYPE_LISTEN`,`ctrler_listen_unpack` 负责解析上报帧,`ctrler_read/connect` 均为空实现)。

## 接口

`internal/driver/driver.go`,与 `Writer`/`Subscriber` 同属**可选能力接口**,通过类型断言检测:

```go
// Listener 是南向驱动的可选监听能力:实现它的 Conn 表示网关被动 listen,
// 设备主动连入并上报数据。scheduler 检测到后注册一次 Listen,数据到达时
// 经 onData 推送 DataPoint,不再按 intervalMs 定时 Read。
type Listener interface {
    Listen(ctx context.Context, points []model.Point, onData func(model.DataPoint)) error
}
```

**语义约定:**

- **设备路由信息**在 `Open` 时经 `DeviceParams` 传入并封存在 `Conn` 内(如从机地址),`Listen` 只注册该设备的点位。这样 `Listen` 的签名与 `Subscriber` 保持一致,Core 无需感知"设备如何被识别"。
- **同一 Connection 的多个设备共享底层监听 socket**:首次 `Listen` 惰性建立 listener,后续设备复用;帧到达后驱动按协议字段(如 UnitID)路由到对应设备。
- **ctx 取消**(配置热加载 / 进程关闭)后,实现须停止监听并释放资源。
- `intervalMs` 在监听模式下被忽略(数据由设备推,无采集周期)。

## 与参考工程 modbus_listen 的映射

| 参考工程(C) | 本工程(Go) |
|---|---|
| Controller 的 `tcp_ip/tcp_port`(监听地址) | `Connection.config.listen` |
| Reg 的 `acAddr`(从机地址) | `Device.params.slaveId` |
| Reg 的 `iOffset`(寄存器偏移) | `Point.address`(十进制偏移) |
| Reg 的 `iDataType`(float 字节序) | `Device.params.byteOrder`(支持 ABCD/BADC/CDAB/DCBA,默认 ABCD) |
| Reg 的 `multiple`(系数) | `Point.scale` |
| Reg 的 `calibration`(加性校准) | ⚠️ 当前 `Point` 无此字段,见下方缺口 |
| `ctrler_listen_unpack(buffer, len, callback)` | 驱动内部 `dispatch(frame)` → `listenDevice.deliver(regs)` |

## 参考实现:`internal/driver/modbus_listen`

网关 listen 一个 TCP 端口,设备/主站连入后按 Modbus TCP(MBAP)推帧,驱动:

1. `Open`:解析 `listen` 地址与 `slaveId`,按 `ConnectionID` 取得共享监听器(引用计数)。
2. `Listen`:把该设备点位注册进共享监听器的 `devices[slaveId]`,首次调用惰性 `net.Listen` 并启动 accept 循环。
3. accept 到连接后逐帧读取(MBAP 6 字节头 + length 字节),按帧头 `UnitID` 路由到设备。
4. `listenDevice.deliver`:按点位地址(寄存器偏移)、设备字节序与 `dataType` 解码(`int16/uint16` 单寄存器恒为大端,`int32/uint32/float32` 跨 2 寄存器按 `byteOrder` 组合),应用 `scale`,回调 `onData` 推送 `DataPoint`。

配置示例:

```jsonc
// Connection: driver = "modbus_listen"
{ "listen": ":502", "timeout": "60s" }

// Device.params
{ "slaveId": 1, "byteOrder": "ABCD" }

// Point: address 为寄存器偏移(十进制),int32/uint32/float32 占 2 个寄存器
{ "name": "level", "address": "0", "dataType": "float32", "scale": 0.1 }
```

## 已知缺口(留待后续)

- **加性校准**:参考工程的 reg 有 `value = raw * multiple + calibration`,本工程 `Point` 只有乘性 `scale`,缺加性 `calibration`。落地真实协议时需给 `Point` 加字段(涉及 SQLite 列与 API),或把校准值编码进 `address`/`params`。
- **监听连接回收**:设备连接的 goroutine 靠读超时回收;reload 时旧监听 socket 关闭,但已建立的连接要等到超时才释放(进程内短暂残留,可接受)。
