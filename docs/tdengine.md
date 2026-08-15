# TDengine 北向输出

TDengine 对接作为一个**北向输出插件** `internal/output/tdengine`,与 `mqtt`、`thingsboard` 并列,实现 `output.Output` 接口,插入 pipeline 的 outputs 列表:

```
scheduler → pipeline ──┬──▶ output/mqtt          (自定义 JSON)
                       ├──▶ output/thingsboard   (ThingsBoard MQTT Gateway)
                       └──▶ output/tdengine      (TDengine REST,taosAdapter)
```

## 1. 接入方式

采用 TDengine 官方 **taosAdapter REST API**(`POST /rest/sql`),不引入 CGO 驱动,保持网关"纯 Go、无 CGO"的约束;一次 HTTP 请求执行一条 SQL,Basic 鉴权。

- 默认地址 `http://127.0.0.1:6041`(taosAdapter 默认端口)。
- 配置 `tdengine.url` 后即启用(可与 mqtt / thingsboard 输出并存)。

## 2. 数据模型

TDengine 是强类型时序库,而网关的 `DataPoint.Value` 是协议无关的 `interface{}`。映射策略:

**超级表(STable)统一存储所有点位**,值按 Go 类型写入对应的强类型列:

| DataPoint.Value 类型 | 列 | 说明 |
|---|---|---|
| bool | `v_bool BOOL` | |
| int8/16/32/64、uint8/16/32/64 | `v_int BIGINT` | 统一到 64 位整数 |
| float32/float64 | `v_double DOUBLE` | |
| string | `v_str NCHAR(4096)` | |
| nil(无值/bad) | 四列全 NULL | 仅保留 `quality` 列 |

每行固定列:`ts TIMESTAMP` + `quality NCHAR(16)` + 上述四个值列(按类型四选一,其余 NULL)。

**子表按 (deviceID, point) 自动建表**,设备名与点位名作为 TAGS(`device_id`、`point`),便于按设备/点位过滤与分区。子表名由 (deviceID, point) 经 FNV-1a hash 派生(稳定、合法、无非法字符),可读的设备/点位信息由 TAGS 承载。

### 建表语句(插件启动时幂等执行)

```sql
CREATE DATABASE IF NOT EXISTS `iot_gateway`;
CREATE STABLE IF NOT EXISTS `iot_gateway`.`data_points` (
  `ts` TIMESTAMP, `quality` NCHAR(16),
  `v_double` DOUBLE, `v_int` BIGINT, `v_bool` BOOL, `v_str` NCHAR(4096)
) TAGS (`device_id` NCHAR(128), `point` NCHAR(128));

-- 首次见到某 (device, point) 时:
CREATE TABLE IF NOT EXISTS `iot_gateway`.`t_<hash>` USING `iot_gateway`.`data_points` TAGS ('sensor-01','temperature');

-- 写入(多行合并为一条 INSERT):
INSERT INTO `iot_gateway`.`t_<hash>` (`ts`,`quality`,`v_double`,`v_int`,`v_bool`,`v_str`)
VALUES (1700000000000,'good',25.5,NULL,NULL,NULL) (1700000001000,'good',26,NULL,NULL,NULL);
```

## 3. 配置

```yaml
tdengine:
  url: "http://127.0.0.1:6041"  # taosAdapter REST 地址(必填,配置后启用)
  username: "root"               # 默认 root
  password: "taosdata"           # 默认 taosdata
  database: "iot_gateway"        # 库名,默认 iot_gateway
  stable: "data_points"          # 超级表名,默认 data_points
  flushInterval: "1s"            # 微批聚合 flush 间隔,默认 1s
```

## 4. 写入路径与批量

- `Publish` 只把 DataPoint 追加进内存缓冲,**不阻塞采集侧**(与 thingsboard 相同的背压隔离)。
- flusher goroutine 按 `flushInterval` 周期性 flush:取走当前缓冲 → 按 (deviceID, point) 分组 → 每组一条多行 INSERT。
- 无值(bad/uncertain)的点也写入,以 `quality` 列记录数据质量,便于时序分析发现采集断点。

## 5. 可靠性与局限

- **失败即丢弃**:单组 INSERT 失败仅记录日志并丢弃(不重试),保持与 mqtt/thingsboard 一致;REST 写入本身可重试,后续版本可加本地缓存补传。
- **无断网缓冲**:TDengine 不可达期间数据丢失(未持久化到本地),与 ThingsBoard P3 的"断网本地补传"属同类待办。
- **每点一子表**:点位数量多时子表数量线性增长;TDengine 子表为轻量元数据,工业网关点位规模(数百~数千)下无压力。
- 字符串值上限 NCHAR(4096),超出会被 TDengine 拒绝;工业点位值通常远小于此。
- 尚未对真实 TDengine 验证(需 taosAdapter + 实例),SQL 语义以 TDengine 3.x 文档为准。

## 6. 查询示例

```sql
-- 某设备某点位最近 100 条
SELECT ts, quality, v_double, v_int, v_bool, v_str
FROM iot_gateway.data_points
WHERE device_id = 'sensor-01' AND point = 'temperature'
ORDER BY ts DESC LIMIT 100;

-- 某设备全部点位(按 TAGS 过滤)
SELECT * FROM iot_gateway.data_points WHERE device_id = 'sensor-01';
```
