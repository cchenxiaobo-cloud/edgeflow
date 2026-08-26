# EdgeFlow

EdgeFlow 是一个类 KubeEdge 的云边协同边缘计算平台，提供设备接入、数据采集、模型分发与弱网通信能力，采用云边两级架构：

- **CloudCore（云端）**：云边通信（WebSocket）、消息路由、节点注册与设备管理（CRD）、**云端分级持久化（v0.4.0 嵌入式 etcd 写穿 / v0.5.0 外部 etcd 模式 / v0.6.0 真多活多副本）**、**模型仓库/版本管理/灰度发布（v0.7.0）**、mTLS 安全通道、REST API 与指标暴露。
- **EdgeCore（边缘端）**：与云端建立安全连接、心跳保活与重连退避、设备数据采集上报、事件总线、模型管理。
- **keadm（安装管理 CLI）**：一键生成云端部署产物与边缘接入产物，支持升级、回滚与证书轮换。

> 当前版本：**v0.12.0**（2026-08-26，digest 校验端到端落地：真实边缘采集闭环 + 发布复核可观测性）。核心能力包括：完整 mTLS（证书签发/CRL/OCSP）、设备 Token 认证、`edgenodes`/`devices`/`devicemodels` CRD、Modbus 设备接入（mapper）、可靠消息投递、弱网重连退避、OPC-UA UA Binary 协议栈（`pkg/opcua`，v0.3.0，明文仅限可信网络）、**云端分级持久化（v0.4.0 嵌入式 etcd / v0.5.0 外部 etcd 模式 / v0.6.0 真多活多副本）**、**模型仓库/版本管理/灰度发布（v0.7.0：模型 API 17 端点，总 HTTP 端点 14→31）**、**外部 etcd RBAC 鉴权透传与终态发布 GC（v0.8.0：L1/L28 闭环）+ 续约失败监控指标**。

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
# {"status":"ok","version":{"version":"v0.8.0","gitCommit":"...","buildTime":"...","goVersion":"go1.26.2"}}

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

每个版本在 [GitHub Releases](https://github.com/cchenxiaobo-cloud/edgeflow/releases) 提供以下制品（以 v0.7.0 为例）：

- 二进制：`cloudcore` / `edgecore` / `keadm` × `darwin-amd64` / `darwin-arm64` / `linux-amd64` / `linux-arm64`（18 个）
- 部署包：`edgeflow-0.7.0.tgz`（Helm Chart）
- 物料：`sbom-0.7.0.json`（SBOM）、`checksums-0.7.0.txt`（sha256 校验清单）

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
| [docs/manual/](docs/manual/) | 用户手册 v0.8.0（[Markdown（GitHub 在线可读）](docs/manual/EdgeFlow-用户手册-v0.8.0.md) · [PDF](docs/manual/EdgeFlow-用户手册-v0.8.0.pdf) · [LaTeX 工程](docs/manual/main.tex)） |
| [docs/solution-manual/](docs/solution-manual/) | 解决方案手册 v1.1.0（[Markdown（GitHub 在线可读）](docs/solution-manual/EdgeFlow-解决方案手册-v1.0.0.md) · [PDF（36 页）](docs/solution-manual/latex/EdgeFlow-解决方案手册-v1.0.0.pdf) · [LaTeX 工程](docs/solution-manual/latex/)） |
| [docs/RELEASE-NOTES-v080.md](docs/RELEASE-NOTES-v080.md) | v0.8.0 发布说明（etcd 鉴权/续约监控/分页与 GC） |

## 版本历史

- **v0.12.0**（2026-08-26）：digest 校验端到端落地——真实边缘双通道 digest 采集（声明式 `@sha256:` pin + docker RepoDigests 运行时兜底，仅 Running 上报）+ 发布 digest 复核端点（`GET .../releases/{id}/digest`，一致结论一键对比）+ finish③ 读库 shadow 自赋值修复（F-1，消除 v0.11.0 latent bug；详见 [RELEASE-NOTES-v0112.md](docs/RELEASE-NOTES-v0112.md)）
- **v0.11.0**（2026-08-26）：镜像 digest 级校验（探活固化 mirrorDigest+边缘上报比对，mismatch→failed）+ hb 键重建计数（/metrics 第 8 项）+ Windows 制品入发布矩阵（12→18）+ ValidateMirror scheme 对齐（详见 [RELEASE-NOTES-v0110.md](docs/RELEASE-NOTES-v0110.md)）
- **v0.10.0**（2026-08-26）：设备属性写穿持久化（③ 收官，重启后属性立即可见）+ 发布批内并发（`RELEASE_BATCH_PARALLEL`，默认 1=串行）+ Windows 交叉编译修复（L20b）（详见 [RELEASE-NOTES-v0100.md](docs/RELEASE-NOTES-v0100.md)）
- **v0.9.0**（2026-08-26）：云端状态持久化补全（Pod 状态写穿 `/edgeflow/podstatus/`，重启后 Pod 列表直接可见）+ 发布前镜像存在性探活（R-1：`RELEASE_MIRROR_CHECK` off/warn/fail，默认 off）（详见 [RELEASE-NOTES-v090.md](docs/RELEASE-NOTES-v090.md)）
- **v0.8.0**（2026-08-26）：运维与安全增强——外部 etcd RBAC 鉴权透传（ETCD_USERNAME/PASSWORD，L1）、续约失败监控指标（L12）、模型列表分页（limit/offset + X-Total-Count）与终态发布 GC（L28，默认关）（详见 [RELEASE-NOTES-v080.md](docs/RELEASE-NOTES-v080.md)）
- **v0.7.0**（2026-08-25）：模型仓库/版本管理/灰度发布——云端模型台账（F41）+ 服务端灰度执行器（F42：白名单/按比例、分批、fail-fast、取消、逆序回滚），模型 API 17 端点（总 HTTP 端点 14→31），边缘零改动（详见 [RELEASE-NOTES-v070.md](docs/RELEASE-NOTES-v070.md)）
- **v0.6.0**（2026-08-25）：真多活——外部 etcd 模式多副本 active-active（租约判活/GuardedDelete/CAS/领跑锁），/healthz 多副本语义（详见 [RELEASE-NOTES-v060.md](docs/RELEASE-NOTES-v060.md)）
- **v0.5.0**（2026-08-24）：外部 etcd 模式——`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 直连共享集群，TLS/mTLS 与明文护栏，启动探活（详见 [RELEASE-NOTES-v050.md](docs/RELEASE-NOTES-v050.md)）
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
