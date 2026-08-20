# iot-gateway-go 多阶段构建:前端 + 静态网关二进制 + 精简运行镜像
#
# 构建:  docker build -t iot-gateway .
# 运行:  docker run -d --name iot-gateway -p 8080:8080 \
#           -v $PWD/gateway.docker.yaml:/data/config.yaml:ro \
#           -v iot-gateway-data:/data iot-gateway
# 或:    docker compose -f docker-compose.example.yml up -d --build
#
# 详细配置与目录约定见 docs/deployment.md §5(Docker 部署)。

# 1) 前端产物(经 go:embed 内嵌进二进制)
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# 2) 编译网关(CGO_ENABLED=0 纯静态二进制,无 glibc 依赖)
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X iot-gateway-go/internal/version.Version=${VERSION}" \
    -o /out/gateway ./cmd/gateway

# 3) 运行镜像(精简,仅二进制 + CA/时区)
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 1000 iotgw
WORKDIR /app
COPY --from=build /out/gateway /app/gateway
COPY config.example.yaml /app/config.example.yaml
RUN mkdir -p /data && chown iotgw:iotgw /data
# 非 root 运行
USER iotgw
EXPOSE 8080
# 数据/日志挂载点;配置放 /data/config.yaml(sqlitePath/log 请指向 /data,见部署文档)
VOLUME ["/data"]
ENTRYPOINT ["/app/gateway"]
CMD ["/data/config.yaml"]
