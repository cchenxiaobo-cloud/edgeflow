# EdgeFlow 项目 Makefile
#
# 常用命令：
#   make build    # 编译全部二进制到 bin/ 目录
#   make test     # 运行单元测试
#   make run      # 本地运行 cloudcore（占位程序）
#   make lint     # 代码静态检查（需要 golangci-lint，未安装时给出提示）
#   make clean    # 清理编译产物

# 二进制输出目录
BIN_DIR := bin

# 编译参数：注入版本号，后续扩展业务时可在这里追加 -ldflags
LDFLAGS := -s -w

.PHONY: build test run lint clean

## build: 编译 cloudcore 和 edgecore 两个二进制
build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/cloudcore ./cmd/cloudcore
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/edgecore ./cmd/edgecore
	@echo "build done: $(BIN_DIR)/cloudcore, $(BIN_DIR)/edgecore"

## test: 运行全部单元测试（含 -race 竞态检测）
test:
	go test -race -v ./...

## run: 本地运行 cloudcore 占位程序
run:
	go run ./cmd/cloudcore

## lint: 静态检查（依赖 golangci-lint，未安装时跳过并提示）
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint 未安装，跳过 lint（安装方式见 README）"; \
	fi

## clean: 清理编译产物
clean:
	rm -rf $(BIN_DIR)
