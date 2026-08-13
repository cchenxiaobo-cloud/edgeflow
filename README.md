# EdgeFlow

EdgeFlow 是一个类 KubeEdge 的边缘计算项目，采用云边两级架构：

- **CloudCore（云端）**：运行在云端，未来负责云边通信（WebSocket）、消息路由（NATS）、设备管理（CRD）等。
- **EdgeCore（边缘端）**：运行在边缘节点，未来负责与云端建立连接、收发消息、上报设备数据等。

> 当前进度：M0 核心模块已完成 —— 基础共享库（version / log / httpx）+ cloudcore 最小可运行服务（提供 `/healthz` 健康检查）；edgecore 仍为占位程序，业务逻辑在后续迭代中实现。

## 目录结构

参考 [golang-standards/project-layout](https://github.com/golang-standards/project-layout)，保持最小：

```
edgeflow/
├── cmd/
│   ├── cloudcore/        # 云端组件入口（常驻服务，提供 /healthz）
│   └── edgecore/         # 边缘端组件入口（占位程序）
├── pkg/
│   ├── version/          # 版本信息（编译时通过 -ldflags 注入）
│   ├── log/              # 轻量日志封装（Info/Warn/Error，基于标准库）
│   └── httpx/            # HTTP 公共处理器（/healthz 健康检查）
├── apis/                 # API 定义（如设备 CRD，后续填充）
├── build/                # 构建产物与镜像相关文件（后续填充）
├── docs/                 # 文档
├── hack/                 # 开发脚本（后续填充）
├── .github/workflows/    # CI 流水线
├── go.mod
├── Makefile
└── README.md
```

## 环境要求

- Go 1.26+
- Make
- golangci-lint（可选，用于静态检查）

## 快速开始

```bash
# 1. 编译全部二进制（输出到 bin/，自动注入版本号）
make build

# 2. 启动云端组件（默认监听 8080 端口）
./bin/cloudcore
# 开发时也可直接运行：go run ./cmd/cloudcore

# 3. 验证健康检查（另开一个终端执行）
curl http://127.0.0.1:8080/healthz
# 期望返回 HTTP 200 和 JSON，例如：
# {"status":"ok","version":{"version":"v0.1.0","gitCommit":"...","buildTime":"...","goVersion":"go1.26.2"}}

# 4. 查看版本信息
./bin/cloudcore --version

# 5. 运行单元测试（含竞态检测与覆盖率）
make test

# 6. 静态检查
make lint
```

### 配置监听端口

`cloudcore` 的监听端口按以下优先级确定：

1. 命令行参数：`./bin/cloudcore --port 9090`
2. 环境变量：`EDGEFLOW_CLOUDCORE_PORT=9090 ./bin/cloudcore`
3. 默认值：`8080`

### 优雅退出

启动后按 `Ctrl+C`（SIGINT）或执行 `kill -TERM <进程号>`（SIGTERM），cloudcore 会停止接收新请求，等待正在处理的请求完成（最多 5 秒）后退出。

手动运行边缘端占位程序：

```bash
go run ./cmd/edgecore
```

## License

TODO: 待补充开源协议。
