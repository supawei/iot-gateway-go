# iot-gateway-go 构建脚本
# 支持三平台交叉编译：x86_64 / aarch64 / arm32
# 依赖全为纯 Go（modernc.org/sqlite 无 CGO），关闭 CGO 即产出静态二进制

BINARY_NAME  := gateway
MAIN_PACKAGE := ./cmd/gateway
DIST_DIR     := dist

GO := go

# 关闭 CGO：依赖全为纯 Go，静态编译便于在无 libc 的网关盒上部署
CGO_ENABLED := 0
# 剥离调试符号与 DWARF，缩小二进制体积（工业网关存储/带宽有限）
LDFLAGS := -s -w

# arm32 软浮点 ABI 版本：7 = Cortex-A 系列（工业网关主流），6 = 旧款 ARMv6
ARM32_GOARM := 7

.PHONY: all build build-all web linux-amd64 linux-arm64 linux-arm32 \
        run test vet fmt fmt-check tidy check clean help

all: build

# 前端产物经 go:embed 打进二进制,构建前先编译前端
build: web ## 构建当前平台，产物置于仓库根目录
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) $(MAIN_PACKAGE)

build-all: web linux-amd64 linux-arm64 linux-arm32 ## 交叉编译三平台，产物置于 dist/<platform>/

web: ## 构建前端 (web/dist,供 go:embed 内嵌)
	cd web && npm install && npm run build

# 产物布局：dist/<platform>/gateway —— 用目录区分架构，二进制名始终保持一致
# 部署时整目录拷贝到目标机，systemd/Docker 路径跨架构统一
linux-amd64:
	@mkdir -p $(DIST_DIR)/$@
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=amd64 \
		$(GO) build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$@/$(BINARY_NAME) $(MAIN_PACKAGE)
	@echo "✓ $(DIST_DIR)/$@/$(BINARY_NAME)"

linux-arm64:
	@mkdir -p $(DIST_DIR)/$@
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=arm64 \
		$(GO) build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$@/$(BINARY_NAME) $(MAIN_PACKAGE)
	@echo "✓ $(DIST_DIR)/$@/$(BINARY_NAME)"

linux-arm32:
	@mkdir -p $(DIST_DIR)/$@
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=arm GOARM=$(ARM32_GOARM) \
		$(GO) build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$@/$(BINARY_NAME) $(MAIN_PACKAGE)
	@echo "✓ $(DIST_DIR)/$@/$(BINARY_NAME)"

run: ## 运行网关（读取 config.yaml）
	$(GO) run $(MAIN_PACKAGE)

test: ## 运行全部测试
	$(GO) test ./...

vet: ## go vet 静态检查
	$(GO) vet ./...

fmt: ## 格式化源码
	$(GO) fmt ./...

fmt-check: ## 检查格式（不修改文件，CI/提交前用）
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未格式化："; echo "$$unformatted"; exit 1; \
	fi

tidy: ## 整理依赖
	$(GO) mod tidy

check: fmt-check vet test ## 提交前检查：格式 + vet + 测试

clean: ## 清理构建产物
	rm -rf $(BINARY_NAME) $(DIST_DIR)

help: ## 显示此帮助
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
