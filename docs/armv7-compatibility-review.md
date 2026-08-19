# ARM v7 兼容性审核文档

> **状态**:已审核(2026-08-19,静态分析 + 实际交叉编译验证)
> **关联**:[Makefile](../Makefile)(`ARM32_GOARM=7`)、[.goreleaser.yaml](../.goreleaser.yaml)(goarch arm/goarm 7)、[ops-monitoring-design.md](ops-monitoring-design.md)(`sys_linux.go` 32 位类型修复 `ddddc27`)
> **范围**:`GOOS=linux GOARCH=arm GOARM=7` 下的编译与运行时可行性
> **结论**:可编译、可静态链接、依赖全支持;真正的运行风险集中在「GOARM=7 硬浮点对 CPU 的要求」与「32 位下 SQLite 的内存/性能表现」,均为部署验证项,非代码缺陷。

## 1. 结论摘要

该工程面向 ARM v7(32 位 ARM、`GOARM=7`)整体友好:

- ✅ **可编译**:`GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0` 交叉编译零报错,产出 `ELF 32-bit LSB ARM EABI5, statically linked`。
- ✅ **可静态部署**:`CGO_ENABLED=0` 全静态,无 glibc 依赖,任意最小化 rootfs 可直接运行。
- ✅ **依赖全支持**:全部为纯 Go,且 modernc.org/sqlite 官方明确列出 `linux arm` 受支持。
- ⚠️ **运行风险点**(按严重度排序,见 §3):
  1. **GOARM=7 硬浮点要求 CPU 带 VFPv3**,无 VFPv3 的板子启动即退出;
  2. **32 位下 SQLite 内存/性能**,低内存板需观察 RSS;
  3. **WAL 模式 + NAND/SD 闪存**的存储可靠性(与架构无关,但嵌入式部署需关注)。

## 2. 验证方法

| 步骤 | 方法与结果 |
|---|---|
| 交叉编译 | `GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build ./cmd/gateway` → 成功,`file` 确认为 32 位 ARM 静态 ELF,无 dynamic section |
| 静态检查 | `go vet ./...` 全绿;`go test ./...` 全量通过(其中 `internal/store` 测试真实跑通 modernc SQLite WAL 路径) |
| 代码扫描 | 全工程**无 cgo、无汇编(.s)、无 `unsafe`、无 `go:linkname`**;仅 `internal/observability` 有 `linux`/`!linux` 两处 build tag |
| 依赖支持矩阵 | 逐一核对模块源码与官方支持表,见 §5 |
| 运行时行为 | 宿主无 32 位用户态 / 无 QEMU / 无 Docker daemon,**无法本机实际执行 32 位二进制**;运行时行为基于 Go 运行时源码(`checkgoarm`)与依赖官方支持矩阵的静态分析 |

## 3. 潜在风险清单

### 3.1 [中] GOARM=7 硬浮点要求 CPU 带 VFPv3

Go 运行时在**进程启动时**执行 `checkgoarm()`(见 `GOROOT/src/runtime/os_linux_arm.go`),CPU 特性不满足直接拒绝启动:

```
runtime: this CPU has no VFPv3 floating point hardware, so it cannot run
a binary compiled for VFPv3 hard floating point. Recompile adding ,softfloat
to GOARM or changing GOARM to 6.
```

- 现状:Makefile / GoReleaser 固定 `GOARM=7`(注释:「7 = Cortex-A 系列(工业网关主流)」)。`GOARM=7` ≈ **ARMv7 + VFPv3-D16 硬浮点**。
- 大多数 ARMv7 工业网关(Cortex-A5/A7/A8/A9)都带 VFPv3/v4 → **无问题**。
- 以下设备会**启动即退出**:无 VFP 的 ARMv7(ARMv7-M 微控制器、软浮点 Linux 板)、仅 VFPv2 的旧芯片。
- 处置:`Makefile` 中 `ARM32_GOARM ?= 7` 已可被 `make ARM32_GOARM=6` 覆盖;部署前用 `grep vfpv3 /proc/cpuinfo` 确认目标板特性;若目标硬件面不确定,可新增 `linux-arm32-v6` 构建目标。

### 3.2 [低] 32 位下 SQLite 内存/性能(非阻断)

- modernc.org/sqlite v1.56.0 `doc.go` **官方明确列出 `linux arm` 受支持**;libc v1.74.4 带 `ccgo_linux_arm.go`/`capi_linux_arm.go`(`//go:build linux && arm`)。
- 现代 SQLite(3.53)经 ccgo 转译后 32 位下**内存占用偏大、速度偏慢**;32 位进程地址空间有限,数据库文件无法 mmap 超过 2GB。
- 低内存板(如 256MB)需关注 RSS:WAL + 补传队列 + `datapointBufferSize=1024` 缓冲。
- 处置:量产前在目标板做长时间(≥72h)运行观察 RSS 与 SQLite 性能。

### 3.3 [低] WAL 模式 + NAND/SD 闪存(存储问题,与架构无关)

- 本项目 `dsnWithPragmas` 启用 `journal_mode(WAL)` + `busy_timeout(5000)`;WAL 需要写 `-shm` 共享内存文件。
- 放在 NAND/SD 卡上需关注 flash 写放大与掉电一致性;现代内核文件系统(ext4/jffs2/ubifs)基本无碍。

### 3.4 [已排除] 32 位字长相关的类型/对齐/时间

| 疑点 | 结论 |
|---|---|
| 64 位原子对齐 | `scheduler.go` 的 `atomic.Int64` 是**堆上结构体字段**,Go 运行时保证 8 字节对齐(atomic 包内部 `align64` 手段),32 位 ARM 上安全 |
| `syscall.Statfs_t.Bsize` 类型 | 32 位为 `int32`,**已由 commit `ddddc27`(arm32 交叉编译类型错误)修复**,`fsDiskStat` 统一 `int64(stat.Bsize)` |
| 时间戳 | Go `time` 内部 64 位,代码全部以 `RFC3339Nano` 字符串落库,**无 2038 问题** |

## 4. 已确认无风险的点

| 检查项 | 结果 |
|---|---|
| 字节序 | 全部显式 `binary.BigEndian`/`LittleEndian`(Modbus 大端、SparkplugB 小端、protobuf varint 手写),**无本机字节序假设**;产物为 LSB(小端)ARM,与主流一致 |
| 依赖纯 Go 性 | paho.mqtt / gopcua.opcua / grid-x.modbus / grid-x.serial / expr-lang / x.crypto / lumberjack 全为纯 Go;grid-x/serial 用 Go 类型化 `syscall` 封装(arch 无关) |
| TDengine 对接 | taosAdapter **REST API**,无 CGO 原生驱动依赖 |
| 动态库依赖 | 静态链接,无 glibc 依赖(arm 产物无 dynamic section),最小化 rootfs 可跑 |
| build tag | 仅 `linux`/`!linux`,**不会在 arm 上静默丢代码路径** |
| /proc 依赖 | `sys_linux.go` 读 `/proc/self/status`、`/proc/meminfo`,失败时优雅降级(`ok=false`),不崩 |
| 内嵌 Web | `go:embed` 静态资源,arch 无关 |
| 内核版本 | Go 1.25 对 linux/arm 最低要求 Linux 3.2,主流 ARMv7 网关内核均满足 |

## 5. 依赖支持矩阵(linux/arm)

| 依赖 | 版本 | 支持 | 证据 |
|---|---|---|---|
| modernc.org/sqlite | v1.56.0 | ✅ | `doc.go` 官方支持表含 `linux arm`(SQLite 3.53.3) |
| modernc.org/libc | v1.74.4 | ✅ | `ccgo_linux_arm.go`、`capi_linux_arm.go`(`//go:build linux && arm`) |
| modernc.org/memory / mathutil | 间接 | ✅ | 纯 Go,无 arch 特定文件缺失 |
| github.com/gopcua/opcua | v0.9.1 | ✅ | 纯 Go,交叉编译通过 |
| github.com/eclipse/paho.mqtt.golang | v1.5.1 | ✅ | 纯 Go |
| github.com/grid-x/modbus | v0.0.0-2026… | ✅ | 纯 Go |
| github.com/grid-x/serial | v0.0.0-2021… | ✅ | 类型化 syscall,交叉编译通过 |
| github.com/expr-lang/expr | v1.17.8 | ✅ | 纯 Go |
| golang.org/x/crypto / net / sys | 间接 | ✅ | 纯 Go |

## 6. 部署前检查清单(ARMv7 目标板)

1. `uname -m` 应为 `armv7l`;
2. `grep -o vfpv3 /proc/cpuinfo` 确认有 VFPv3;没有则 `make linux-arm32 ARM32_GOARM=6` 重编;
3. 内核版本 ≥ 3.2;
4. 确认 `/proc` 已挂载(否则仅监控指标不显示,不崩);
5. 使用 `dist/gateway_linux_arm_7/gateway`(GoReleaser 已产出:静态、已 strip、~17.6MB)部署,无需安装任何运行库;
6. 首次冒烟:`timeout 20 ./gateway config.yaml`,观察 SQLite 打开、schema 迁移、HTTP 监听日志。

## 7. 建议(可选改进)

- 新增 `linux-arm32-v6`(GOARM=6)构建目标,扩大兼容面(牺牲少量性能);
- 量产前在目标板做 ≥72h 压测,重点观察 RSS 与 SQLite WAL 行为;
- 若目标板对存储可靠性敏感,评估 `journal_mode` 是否改为 `TRUNCATE`(换可靠性,降并发),需权衡 WAL 的读写并发收益。
