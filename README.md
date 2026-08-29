# EdgeFlow

EdgeFlow 是一个类 KubeEdge 的云边协同边缘计算平台，提供设备接入、数据采集、模型分发与弱网通信能力，采用云边两级架构：

- **CloudCore（云端）**：云边通信（WebSocket）、消息路由、节点注册与设备管理（CRD）、**云端分级持久化（v0.4.0 嵌入式 etcd 写穿 / v0.5.0 外部 etcd 模式 / v0.6.0 真多活多副本）**、**模型仓库/版本管理/灰度发布（v0.7.0）**、mTLS 安全通道、REST API 与指标暴露。
- **EdgeCore（边缘端）**：与云端建立安全连接、心跳保活与重连退避、设备数据采集上报、事件总线、模型管理。
- **keadm（安装管理 CLI）**：一键生成云端部署产物与边缘接入产物，支持升级、回滚与证书轮换。

> 当前版本：**v0.24.0**（2026-08-29，MQTT 功能轮——首个功能开发版本：MQTT 3.1.1 协议栈（`pkg/mqtt`，codec+client+通配匹配，-race 绿）、订阅型设备 Mapper（`mappers/mqtt`，EDGEFLOW_MQTT_* 全 opt-in）、进程内测试 broker（`pkg/mqttsim`）+ 真实装配 e2e；另含 §23 残余清理（契约守卫注释口径修正、keadm 产物目录权限 opt-in `EDGEFLOW_JOIN_DIR_MODE` 默认 0755 不变），端点 42 不变零新依赖默认行为兼容）。核心能力包括：完整 mTLS（证书签发/CRL/OCSP）、设备 Token 认证、`edgenodes`/`devices`/`devicemodels` CRD、Modbus 设备接入（mapper，轮询型）、OPC-UA 设备接入（mapper，订阅+轮询）、**MQTT 设备接入（mapper，订阅型，v0.24.0）**、可靠消息投递、弱网重连退避、OPC-UA UA Binary 协议栈（`pkg/opcua`，v0.3.0，明文仅限可信网络）、**MQTT 3.1.1 协议栈（`pkg/mqtt` + `pkg/mqttsim`，v0.24.0）**、**云端分级持久化（v0.4.0 嵌入式 etcd / v0.5.0 外部 etcd 模式 / v0.6.0 真多活多副本）**、**模型仓库/版本管理/灰度发布（v0.7.0：模型 API 17 端点，总 HTTP 端点 14→31）**、**外部 etcd RBAC 鉴权透传与终态发布 GC（v0.8.0：L1/L28 闭环）+ 续约失败监控指标**。

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

- **v0.24.0**（2026-08-29）：MQTT 功能轮——首个功能开发版本：MQTT 3.1.1 协议栈（pkg/mqtt：九种报文 codec + varint/主题双校验 + ErrMalformed 哨兵族 + client 读泵分发/QoS1 PUBACK 等待/KeepAlive/无自动重连由上层负责 + MatchTopic MQTT-4.7 通配；26 测试 -race 绿）+ 订阅型设备 Mapper（mappers/mqtt：EDGEFLOW_MQTT_BROKER/TOPICS/DEVICE_NAME/NAMESPACE/CMD_TOPIC 全 opt-in，payload 三形态容错解析，监管循环断线重连，台账触点全覆盖，9 测试）+ 进程内测试 broker（pkg/mqttsim：CONNECT 校验/SUBACK/分发/PINGREQ/出站队列丢弃计数，9 测试 -race 绿）+ TestMQTTDeviceE2E 真实装配全数据面（e2e 全套 288s 绿）；另含 §23 残余清理（R-1 契约守卫注释口径修正、SEC-03 附 keadm 产物目录权限 opt-in EDGEFLOW_JOIN_DIR_MODE 默认 0755 不变、OPCUA-GUIDE 无漂移确认）；R-5/R-6 残余票登记 KNOWN-ISSUES §24；HTTP 端点保持 42，零新依赖，默认行为与 v0.23.0 兼容；详见 [docs/RELEASE-NOTES-v0240.md](docs/RELEASE-NOTES-v0240.md)

- **v0.21.0**（2026-08-28）：安全默认值包 + 协议纵深包——SEC-01 管理 API 认证关闭启动 WARN（AUTH_WARN=off 可静默）+ SEC-02 云边令牌强校验开关 `EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN=on`（服务端未配令牌时拒绝携带令牌的注册防伪造探测，无令牌注册裸奔兼容，默认关）+ CHN-06 裸奔组合（无 mTLS 且无令牌）聚合告警 + Helm `cloudcore.auth.*`/`cloudcore.cloudhub.*` 段（默认全关）；协议面 PRT-01/14 数组预分配放大防御（声明×最元素字节数 vs 剩余缓冲预检，>1024 元素豁免阈值保持截断语义）+ PRT-03 DiagnosticInfo 递归深度上限 100 + PRT-04 订阅泵退出关闭 pubCh（sync.Once 防双关）+ PRT-18 mapper 订阅自愈；HTTP 端点保持 42，默认行为与 v0.20.0 逐字节一致；详见 [docs/RELEASE-NOTES-v0210.md](docs/RELEASE-NOTES-v0210.md)
- **v0.22.0**（2026-08-28）：P1 缺陷修复包（7 项全闭环）——T-05 重注册事件序+无幽灵节点测试钉死 + T-08 慢客户端字节配额（默认 64MiB，EDGEFLOW_CLOUDHUB_SEND_QUOTA_BYTES）+ RegisterAck 前置（注册风暴退避）+ 广播内存 gauge `edgeflow_cloudcore_hub_send_buffer_bytes` + T-07 边缘下行去重 SQLite 持久化（TTL 24h/上限 1 万条/重启不丢）+ T-10 旧命名容器迁移 Inspect 复核 + T-06 发布终态写点接状态机断言/digest 失败同源失败预算/failFast 与 head 解耦 + T-09 release 子资源跨模型 404 统一（ownedRelease 7 端点）+ canary 独占语义登记 §7.11 + T-11 `EDGEFLOW_CLOUDCORE_CRL_STRICT`/`EDGEFLOW_CLOUDCORE_OCSP_FRESH` 吊销收紧开关；HTTP 端点保持 42，默认行为与 v0.21.0 兼容；详见 [docs/RELEASE-NOTES-v0220.md](docs/RELEASE-NOTES-v0220.md)
- **v0.23.0**（2026-08-28）：P2 缺陷修复包+审计收官（T-12~T-20，台账 71 条全量勾稽 55✅/14📌/2🗑️）——T-12 观测面（订阅丢事件计数+续约队列水位 metrics+listFailed 告警 env+paused kind 日志+deploy/grafana 面板）+ T-13 锁序工程化（ARCHITECTURE §13，全仓无嵌套持有核对）+ T-16 OPC-UA 纵深 13 修（nonce 失败即报错/防重放/OPN 三重校验/ReceiveBufferSize 16MB/失败清理/负长度拒绝/EndpointUrl 4096/Abort 哨兵/pending 清理/sim Stop 收敛/mappers 锁外重连+Stop 快照+withConn 预算门，-race 全绿）+ T-14 契约统一（summary 恒现、releases 列表 ReleaseList 信封化——唯一破坏性变更客户端需改读 items、API-SPEC §7.12 错误码映射）+ T-15 输入卫生（mirror 预检+PathEscape+url.Values、CRD 11 处校验标记、Validate Version ^v[0-9]+$、msg.ID/newReleaseID rand 失败即报错、CRLF 消毒）+ T-17 keadm --token-file+README 脱敏 + T-18 CheckOrigin 白名单（缺 Origin 放行）+Helm etcd 密码 Secret 化+端口校验 + T-19 退避边界测试+CHN-08 LB 超时标注 + T-20 CHN-13 eventbus 确认等待 5s 有界化；HTTP 端点保持 42，默认行为与 v0.22.0 兼容零新依赖；详见 [docs/RELEASE-NOTES-v0230.md](docs/RELEASE-NOTES-v0230.md)
- **v0.20.0**（2026-08-27）：发布生命周期收口——POST .../retry 失败节点克隆重发（RetryOf 审计回指）+ DELETE .../releases/{id} 终态归档删除（在途绝不删）+ releaseNotes 元数据创建期定死全路径透出；HTTP 端点 40→42 只增不改；详见 [docs/RELEASE-NOTES-v0120.md](docs/RELEASE-NOTES-v0120.md)
- **v0.19.0**（2026-08-27）：发布面智能运维第二批——PATCH 白名单扩展 failureBudget（改小/关闸运行中生效）+ GET releases/{id}/snapshot 审计快照一键全景（events+summary+nodes）+ GET /api/v1/releases 全局发布查询（七态过滤·limit≤500·稳定排序）；HTTP 端点 38→40 只增不改；详见 [docs/RELEASE-NOTES-v0119.md](docs/RELEASE-NOTES-v0119.md)
- **v0.18.0**（2026-08-27）：发布面智能运维——failureBudget 失败预算达标自动暂停（复用 paused 状态机可 resume 续跑）+ Events 发布事件时间线（CAS 并发安全·环形 32 条·随快照迁移）+ GET /api/v1/deployments 全局部署影子聚合查询；HTTP 端点 37→38 只增不改；详见 [docs/RELEASE-NOTES-v0118.md](docs/RELEASE-NOTES-v0118.md)
- **v0.17.0**（2026-08-27）：发布任务运维深化——PATCH 运行中可调执行参数（batchSize/pauseBetween/failFast 批边界生效·CAS 安全）+ 发布列表 status 多值过滤 + dryRun 预检（零落盘·TOCTOU 快照口径明示）；HTTP 端点 36→37 只增不改；详见 [docs/RELEASE-NOTES-v0117.md](docs/RELEASE-NOTES-v0117.md)
- **v0.16.0**（2026-08-27）：AI 模型管理深化——定时维护窗口发布（notBeforeMs opt-in）+ 运行中发布 pause/resume（批边界生效/保锁续租）+ 模型目录 export/import（幂等 upsert/active 直通灾备）；HTTP 端点 32→36 只增不改；详见 [docs/RELEASE-NOTES-v0116.md](docs/RELEASE-NOTES-v0116.md)
- **v0.15.0**（2026-08-27）：OPC-UA 里程碑第三阶段——Subscription 订阅推送（EDGEFLOW_OPCUA_SUBSCRIPTION=on，值变化即时采集/断线自动重建/缺省 off 轮询不变）+ Browse 点位发现（hack/opcua-browse 一键输出 NODES 配置行）；TypeIds 经 OPC Foundation 官方表核验；修复试解分派误吞互操作缺陷（详见 [docs/OPCUA-GUIDE.md](docs/OPCUA-GUIDE.md) 与 [docs/RELEASE-NOTES-v0115.md](docs/RELEASE-NOTES-v0115.md)）
- **v0.14.0**（2026-08-27）：OPC-UA 里程碑第二阶段——端到端设备接入闭环：pkg/opcua 补齐 SecureChannel/Session（匿名）/Read/Write 服务与 Client API（零新依赖）+ 自研模拟器 pkg/opcuasim（6 点位动态模型）+ mappers/opcua 采集转换/写点回读/台账 + edgecore 4 env opt-in 装配；零新增端点（32 不变）、老边缘零动作（详见 [docs/OPCUA-GUIDE.md](docs/OPCUA-GUIDE.md)）
- **v0.13.0**（2026-08-26）：模型生命周期与运维收尾——deployments 列表分页（A′，L28 同族收官）+ 删除级联收官（B，GC 开启下删模型级联清终态发布）+ offlineAt 精确展示（C，L16 残余）；零新增 env、零新增端点（详见 [RELEASE-NOTES-v0113.md](docs/RELEASE-NOTES-v0113.md)）
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
