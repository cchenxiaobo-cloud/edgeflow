# EdgeFlow

EdgeFlow 是一个类 KubeEdge 的云边协同边缘计算平台，提供设备接入、数据采集、模型分发与弱网通信能力，采用云边两级架构：

- **CloudCore（云端）**：云边通信（WebSocket）、消息路由、节点注册与设备管理（CRD）、**云端状态持久化（v0.4.0：嵌入式 etcd 写穿，跨重启保留注册台账与设备期望态）**、mTLS 安全通道、REST API 与指标暴露。
- **EdgeCore（边缘端）**：与云端建立安全连接、心跳保活与重连退避、设备数据采集上报、事件总线、模型管理。
- **keadm（安装管理 CLI）**：一键生成云端部署产物与边缘接入产物，支持升级、回滚与证书轮换。

> 当前版本：**v0.4.0**（2026-08-24）。核心能力包括：完整 mTLS（证书签发/CRL/OCSP）、设备 Token 认证、`edgenodes`/`devices`/`devicemodels` CRD、Modbus 设备接入（mapper）、可靠消息投递、弱网重连退避、OPC-UA UA Binary 协议栈（`pkg/opcua`，v0.3.0）、**云端状态持久化（嵌入式 etcd 写穿：注册台账与设备 Desired 跨重启保留；v0.4.0 起）**。

## 目录结构

```
edgeflow/
├── apis/                 # API 定义（edgenodes / devices / devicemodels CRD）
├── build/charts/edgeflow/  # Helm Chart（云端部署）
├── cloud/                # 云端实现（cloudhub、registry、devicecontroller 等）
├── cmd/                  # 入口：cloudcore / edgecore / keadm
├── config/crds/          # CRD 清单
├── docs/                 # 架构、API、部署、安全、手册等文档
├── edge/                 # 边缘端实现（edgehub、eventbus、modelmanager 等）
├── examples/             # 示例
├── hack/                 # 开发脚本（证书生成、冒烟测试等）
├── mappers/              # 设备接入（Modbus 等）
├── pkg/                  # 共享库（certs、opcua、version、log 等）
├── .github/workflows/    # CI 流水线
├── Makefile
└── go.mod
```

## 环境要求

- Go 1.26+
- Make
- golangci-lint（可选，静态检查）

## 快速开始

```bash
# 1. 编译全部二进制（输出到 bin/，自动注入版本号）
make build

# 2. 启动云端组件（默认监听 8080 端口）
./bin/cloudcore

# 3. 验证健康检查
curl http://127.0.0.1:8080/healthz
# 期望返回 HTTP 200 和 JSON，例如：
# {"status":"ok","version":{"version":"v0.4.0","gitCommit":"...","buildTime":"...","goVersion":"go1.26.2"}}

# 4. 运行单元测试（含竞态检测与覆盖率）
make test

# 5. 静态检查
make lint

# 6. 交叉编译（为边缘节点产出生效二进制）
make cross-build
```

### 部署与节点接入

```bash
# 云端：校验并部署 Helm Chart
make helm-lint
helm install edgeflow build/charts/edgeflow/

# 边缘接入：使用 keadm 生成接入产物
./bin/keadm join --cloudcore-ip=<云端IP> --token=<设备Token> --node-id=<节点ID>
```

详细用法见 [docs/KEADM.md](docs/KEADM.md)、[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) 与 [docs/REAL-CLUSTER-GUIDE.md](docs/REAL-CLUSTER-GUIDE.md)。

### 从 Release 安装

每个版本在 [GitHub Releases](https://github.com/cchenxiaobo-cloud/edgeflow/releases) 提供以下制品（以 v0.3.0 为例）：

- 二进制：`cloudcore` / `edgecore` / `keadm` × `darwin-arm64` / `linux-amd64` / `linux-arm64`
- 部署包：`edgeflow-0.3.0.tgz`（Helm Chart）
- 物料：`sbom.json`（SBOM）、`checksums.txt`（sha256 校验清单）

## 文档

| 文档 | 说明 |
|------|------|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 系统架构与模块设计 |
| [docs/API-SPEC.md](docs/API-SPEC.md) | REST API 契约 |
| [docs/KEADM.md](docs/KEADM.md) | keadm 使用说明（init/join/upgrade/rollback/cert rotate） |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | 部署指南 |
| [docs/SECURITY.md](docs/SECURITY.md) | 安全机制（mTLS/Token/CRL/OCSP） |
| [docs/MAPPER-GUIDE.md](docs/MAPPER-GUIDE.md) | Mapper 开发指南 |
| [docs/KNOWN-ISSUES.md](docs/KNOWN-ISSUES.md) | 已知问题台账 |
| [docs/manual/](docs/manual/) | 用户手册（LaTeX 工程 + [PDF 下载](docs/manual/EdgeFlow-用户手册-v0.1.0.pdf)） |
| [docs/solution-manual/](docs/solution-manual/) | 解决方案手册（[v1.0.0 PDF](docs/solution-manual/EdgeFlow-解决方案手册-v1.0.0.pdf) · [v1.0.0 Markdown](docs/solution-manual/EdgeFlow-解决方案手册-v1.0.0.md) · [v1.0.2 HTML](docs/solution-manual/EdgeFlow-解决方案手册-v1.0.2.html)） |
| [docs/RELEASE-NOTES-v030.md](docs/RELEASE-NOTES-v030.md) | 各版本发布说明 |
| [docs/RELEASE-NOTES-v040.md](docs/RELEASE-NOTES-v040.md) | v0.4.0 发布说明（云端持久化） |

## 版本历史

- **v0.4.0**（2026-08-24）：云端状态持久化——嵌入式 etcd 写穿（注册台账与设备 Desired 跨重启保留），Helm PVC + 资源上调（详见 [RELEASE-NOTES-v040.md](docs/RELEASE-NOTES-v040.md)）
- **v0.3.0**（2026-08-19）：KNOWN-ISSUES 闭环 + OPC-UA UA Binary 协议栈第一阶段
- **v0.2.0**（2026-08-18）：功能增量（详见 [RELEASE-NOTES-v020.md](docs/RELEASE-NOTES-v020.md)）
- **v0.1.1**（2026-08-18）：安全加固 + 收尾（详见 [RELEASE-NOTES-v011.md](docs/RELEASE-NOTES-v011.md)）
- **v0.1.0**（2026-08-14）：首个可运行版本（详见 [RELEASE-NOTES-v0.1.0.md](docs/RELEASE-NOTES-v0.1.0.md)）

## 安全说明

- 云边通信使用 mTLS（证书签发/CRL/OCSP）与设备 Token 认证，详见 [docs/SECURITY.md](docs/SECURITY.md)。
- `pkg/opcua` 当前仅支持 SecurityPolicy None（明文），**仅限可信隔离网络使用，严禁暴露到不可信网络**。

## License

[Apache License 2.0](LICENSE)
