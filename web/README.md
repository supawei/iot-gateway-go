# iot-gateway-go 管理界面

网关的 Web 管理前端,独立工程,基于 **Vue 3 + Element Plus + Vite**。通过网关的 REST API 管理连接、设备、点位,并查看设备运行状态。

## 功能

- **概览**:设备总数 / 在线数 / 连接数统计 + 设备实时状态表(在线、最近采集、最近错误)。
- **连接**:连接(Connection)的增删改查,配置以 JSON 编辑(modbus / modbus_listen / opcua)。
- **设备**:设备(Device)的增删改查、点位(point)动态编辑、克隆、即时写值。

## 开发

```bash
npm install
npm run dev        # http://localhost:5173, /api 代理到 http://localhost:8080
```

启动前先运行后端网关(`./gateway`,默认 `:8080`)。如需改后端地址,编辑 `vite.config.js` 的 proxy target。

## 构建与部署

```bash
npm run build      # 产物在 dist/
```

`dist/` 会被 Go 侧的 `web/embed.go` 通过 `go:embed` **打进网关二进制**,由网关进程在同端口直接提供界面与 API,部署时**无需 nginx / 静态服务器**:

```bash
# 在仓库根目录:构建前端 + 编译二进制(前端产物已内嵌)
make build
# 或分步:
make web && go build -o gateway ./cmd/gateway
```

启动 `./gateway` 后,浏览器访问 `http://<网关IP>:8080/` 即为管理界面,API 在同端口的 `/api/v1`。

> 注:`go build` 依赖 `web/dist` 存在(仓库已提交构建产物,克隆后可直接编译);改动前端源码后先 `npm run build`(或 `make web`)再编译。

## 目录

```
src/
  main.js            入口
  App.vue            侧边栏布局
  router/index.js    路由
  api/index.js       REST 封装(axios)
  styles.css         主题覆盖
  views/
    Dashboard.vue    概览
    Connections.vue  连接管理
    Devices.vue      设备管理
```
