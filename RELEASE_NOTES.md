# v0.1.0 发布说明

> **首个公开预览版**(0.x 阶段):功能已成型,但仍在演进,API / 行为可能变化。
> 计划在真实实例 E2E 全部通过后发布 v1.0.0,届时冻结 API 与 schema(见 [ROADMAP](ROADMAP.md))。

## 核心能力

- **南向**:Modbus(RTU / TCP / RTU over TCP)、OPC UA(轮询 + 订阅推送)、modbus_listen(设备主动连入)。
- **北向**:MQTT、ThingsBoard、TDengine、smardaten(云平台全双工)、Sparkplug B。
- **中台**:设备-点位模型、REST 配置 API、Web 管理界面(内嵌进二进制,免 nginx)、增量热加载、断网补传、边缘计算(过滤 / 聚合)、规则告警(跨设备表达式)、API 鉴权(scope/RBAC)。
- **运维**:`/metrics` + `/livez` + `/readyz`、结构化日志(component/gateway_id)、systemd 单元、Docker 镜像、部署文档。

## 性能基线(2026-08-20 压测,详见 [docs/scale-testing.md §6.2](docs/scale-testing.md))

| 场景 | 结果 |
|---|---|
| 2000 设备 @1s | 2000 次采集/s,0 错误 |
| 2000 设备 @500ms | 4000 次/s |
| 2000 设备 ×4 点 ×20 连接 | 8000 点/s |
| 同上 + MQTT 北向(QoS1) | 7990 条/s,零丢零积压 |
| RSS / goroutines | ~36MB / 80(与设备数解耦) |

## 部署

解压归档后 `./gateway` 直接启动(`config.yaml` 已内置,日志目录自动创建);systemd 见 `deploy/gateway.service`;完整部署 / 升级 / ARMv7 说明见 [docs/deployment.md](docs/deployment.md)。

## 已知边界(0.1.0 未验证项,见 [ROADMAP A1](ROADMAP.md))

- ThingsBoard / TDengine / OPC UA 安全模式 / smardaten / Sparkplug B 的**真实实例 E2E 待验证**;
- ARMv7 真机内存上限待实测;
- 断连韧性 / 告警开销 / MQTT 批量模式待补压测。
