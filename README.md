# EdgeFlow

EdgeFlow 是一个类 KubeEdge 的边缘计算项目骨架，采用云边两级架构：

- **CloudCore（云端）**：运行在云端，未来负责云边通信（WebSocket）、消息路由（NATS）、设备管理（CRD）等。
- **EdgeCore（边缘端）**：运行在边缘节点，未来负责与云端建立连接、收发消息、上报设备数据等。

> 当前仓库仅包含最小可运行骨架（两个占位程序），业务逻辑在后续迭代中实现。

## 目录结构

参考 [golang-standards/project-layout](https://github.com/golang-standards/project-layout)，保持最小：

```
edgeflow/
├── cmd/
│   ├── cloudcore/        # 云端组件入口
│   └── edgecore/         # 边缘端组件入口
├── pkg/                  # 可复用的业务代码库（后续填充）
├── apis/                 # API 定义（如设备 CRD，后续填充）
├── build/                # 构建产物与镜像相关文件（后续填充）
├── docs/                 # 文档（后续填充）
├── hack/                 # 开发脚本（后续填充）
├── .github/workflows/    # CI 流水线
├── go.mod
├── Makefile
└── README.md
```

## 环境要求

- Go 1.26+
- Make

## 快速开始

```bash
# 1. 编译全部二进制（输出到 bin/）
make build

# 2. 运行云端占位程序
make run
# 或直接：go run ./cmd/cloudcore

# 3. 运行单元测试
make test

# 4. 静态检查（需要 golangci-lint，可选）
# 安装：go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
make lint
```

手动运行边缘端占位程序：

```bash
go run ./cmd/edgecore
```

## License

TODO: 待补充开源协议。
