# EdgeFlow 项目 Makefile
#
# 常用命令：
#   make build    # 编译全部二进制到 bin/ 目录
#   make test     # 运行单元测试（含竞态检测和覆盖率）
#   make run      # 本地运行 cloudcore
#   make lint     # 代码静态检查（需要 golangci-lint，未安装时给出提示）
#   make clean    # 清理编译产物

# 二进制输出目录
BIN_DIR := bin

# 版本信息：默认 v0.1.0，可用 make build VERSION=v0.2.0 覆盖
VERSION ?= v0.1.0
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date +%Y-%m-%dT%H:%M:%S%z)

# 编译参数：通过 -ldflags 把版本信息注入到 pkg/version 包
LDFLAGS := -s -w \
	-X edgeflow/pkg/version.Version=$(VERSION) \
	-X edgeflow/pkg/version.GitCommit=$(GIT_COMMIT) \
	-X edgeflow/pkg/version.BuildTime=$(BUILD_TIME)

.PHONY: build test run lint clean

## build: 编译 cloudcore 和 edgecore 两个二进制
build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/cloudcore ./cmd/cloudcore
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/edgecore ./cmd/edgecore
	@echo "build done: $(BIN_DIR)/cloudcore, $(BIN_DIR)/edgecore"

## test: 运行全部单元测试（含竞态检测和覆盖率）
test:
	go test -race -cover ./...

## run: 本地运行 cloudcore
run:
	go run ./cmd/cloudcore

## lint: 静态检查（依赖 golangci-lint，未安装时给出提示）
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint 未安装，跳过 lint（安装方式见 README）"; \
	fi

## clean: 清理编译产物
clean:
	rm -rf $(BIN_DIR)
