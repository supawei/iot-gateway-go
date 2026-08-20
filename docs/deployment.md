# 部署与升级

> **状态**:2026-08-20(v1.0.0 发布冲刺,对应 ROADMAP Phase A4)
> **关联**:[ROADMAP 发布与演进路线](../ROADMAP.md)、[config.example.yaml](../config.example.yaml)、[armv7-compatibility-review.md](armv7-compatibility-review.md)
> **范围**:裸机(systemd)部署为主,Docker 为可选;含 v1.0.1 起的版本化升级流程。

## 1. 产物与平台

GoReleaser 发布产出于 GitHub Releases,三平台静态二进制(`CGO_ENABLED=0`,无 glibc 依赖):

| 平台 | 目录/包 | 说明 |
|---|---|---|
| linux amd64 | `gateway_linux_amd64` | x86_64 工控机/服务器 |
| linux arm64 | `gateway_linux_arm64` | aarch64 网关盒 |
| linux armv7 | `gateway_linux_arm_7` | 32 位 ARM(Cortex-A,工业网关主流) |

发布归档内容:`gateway`(二进制,前端已内嵌)+ `config.yaml`(默认配置,**解压后可直接 `./gateway` 启动**)+ `deploy/gateway.service` + `docs/deployment.md` + `README.md`/`LICENSE`。本地构建:`make build`(当前平台,产物在根目录)或 `make build-all`(三平台,产物在 `dist/<platform>/gateway`)。

## 2. 目录布局(推荐)

```
/opt/iot-gateway/gateway          # 可执行文件(单文件,前端已内嵌)
/etc/iot-gateway/config.yaml      # 配置(引导项:HTTP/存储路径/日志;业务配置在 SQLite)
/var/lib/iot-gateway/gateway.db   # SQLite 数据(+ -wal/-shm),sqlitePath 相对此目录
/var/lib/iot-gateway/logs/        # 日志文件(如启用 log.file)
```

systemd 单元 `deploy/gateway.service` 已按此布局写好。

## 3. 裸机部署(systemd)

```bash
# 1) 目录与二进制(替换 <arch> 为 amd64/arm64/arm_7)
install -d -m 0755 /opt/iot-gateway /etc/iot-gateway /var/lib/iot-gateway
install -m 0755 dist/linux-<arch>/gateway /opt/iot-gateway/gateway

# 2) 配置:发布归档已自带可直接使用的 config.yaml(默认值即开箱即用);
#    源码构建则先 cp config.example.yaml config.yaml
install -m 0644 config.yaml /etc/iot-gateway/config.yaml
#    按需编辑 sqlitePath / log.file / scheduler.poolSize(见 config.yaml 内注释)

# 3) systemd 单元
install -m 0644 deploy/gateway.service /etc/systemd/system/iot-gateway.service
systemctl daemon-reload
systemctl enable --now iot-gateway

# 4) 验证
systemctl status iot-gateway
curl -fsS http://localhost:8080/readyz      # 200 = 就绪
journalctl -u iot-gateway -f                # 日志(文件日志另按 config.yaml)
```

Web 管理界面:`http://<主机>:8080/`,默认管理员 `admin/admin123`(首次登录强制改密)。鉴权默认启用;逃生舱为 `auth.enabled: false`(不推荐生产)。

## 4. 升级流程(v1.0.1 起)

> v1.0.0 无版本化迁移需求;从 v1.0.1 起,库 schema 版本由 `PRAGMA user_version` 记录,
> 网关启动时自动逐级迁移(单事务、失败回滚),**升级时无需手工改库**。

```bash
systemctl stop iot-gateway
# 备份数据(强烈建议,迁移虽可回滚但备份是保险)
cp -a /var/lib/iot-gateway/gateway.db* /var/lib/iot-gateway/backup-$(date +%F)/ 2>/dev/null || true
# 替换二进制
install -m 0755 dist/linux-<arch>/gateway /opt/iot-gateway/gateway
systemctl start iot-gateway
systemctl status iot-gateway      # 确认 running 且无 schema 相关报错
curl -fsS http://localhost:8080/readyz
```

回滚:若新版本异常,用备份恢复 `gateway.db*` 后换回旧二进制重启即可;库版本比二进制新时网关会**拒绝启动**(提示需升级二进制),不会静默破坏数据。

## 5. Docker 部署(可选)

```bash
# 准备配置(基于 config.example.yaml,数据/日志指向 /data 卷):
#   storage: { sqlitePath: "/data/gateway.db" }
#   log:     { file: { path: "/data/logs/gateway.log", ... } }
# 写入 gateway.docker.yaml

docker build -t iot-gateway .
docker run -d --name iot-gateway --restart unless-stopped \
  -p 8080:8080 \
  -v $PWD/gateway.docker.yaml:/data/config.yaml:ro \
  -v iot-gateway-data:/data \
  iot-gateway

# 或 Compose
docker compose -f docker-compose.example.yml up -d --build
```

镜像内以非 root 用户运行(`iotgw`),数据全在 `/data` 卷内持久化。升级 = 重新 `docker build` + `docker compose up -d`(数据卷不动,schema 由启动迁移处理)。

## 6. ARMv7 网关盒要点

- 交叉编译产物自带;**需硬件支持 VFPv3**(`GOARM=7` 硬浮点);旧款 ARMv6 用 `make ARM32_GOARM=6 build-all` 重编;
- **内存优先**:32 位 SQLite 转译库内存占用偏大,建议压测确认规模上限(见 [scale-testing.md §7](scale-testing.md)),必要时降点位/降频/减小 `poolSize`;
- **闪存与 WAL**:SQLite 默认 WAL 模式,建议把数据目录放**可写寿命够的存储**;断电频繁场景留意 `-wal` 文件(补传/告警为主要写源,`backfillMax` 可限流);
- 时钟与时区:数据打点用本地时间,建议 `timedatectl set-timezone Asia/Shanghai` 并保持 NTP 同步。

## 7. 安全加固

- **鉴权**:默认开启。上线前改掉默认 `admin/admin123`(首登强制改密),创建按需的 API Key(scope/RBAC 见 [docs/authz.md](authz.md));
- **网络**:`http.addr` 默认监听 `:8080`(全接口),仅内网部署可保持;暴露公网应置于反向代理 + HTTPS;
- **非 root 运行**(可选):`useradd -r -s /usr/sbin/nologin iotgw`,改单元 `User/Group=iotgw`,并把 `/var/lib/iot-gateway` 属主改为 `iotgw`(`chown -R iotgw:iotgw`);若南向需访问串口设备,把用户加入 `dialout` 组;
- **备份**:周期性备份 `gateway.db*`(或干脆停服拷文件),配合升级回滚。

## 8. 常见问题

| 现象 | 处理 |
|---|---|
| `curl /readyz` 非 200 | `journalctl -u iot-gateway -n 50`;大概率配置/端口/存储路径问题 |
| 启动报"数据库 schema 版本比二进制新" | 用旧库跑了新二进制/回滚了二进制;升级二进制或还原库 |
| 启动报"表结构不匹配…重建" | 发布前旧开发库形态不一致;按提示删除 db 文件重建(仅开发期) |
| 日志不落文件 | 检查 `log.file.path` 目录可写(默认相对 WorkingDirectory) |
| 南向串口权限 | systemd 下 `User` 需加入 `dialout` 组;容器需 `--device /dev/ttyS0` 透传 |
| 端口被占 | 改 `http.addr`;systemd 下同时留意 `LimitNOFILE` 是否过低 |
