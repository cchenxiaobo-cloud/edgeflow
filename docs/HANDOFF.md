# EdgeFlow 开发者指南（v0.1.0 定稿）

> - 对应 ROADMAP WBS 9.4「开发者指南」：代码结构、开发流程、测试规范、贡献指南、常见任务。
> - 状态：✅ **v0.1.0 定稿**（2026-08-14）。评审记录见 `docs/REVIEWS.md`（9.4 评审归档）。
> - 目的：让后续开发者（包括零基础接手人）能按本文档独立接过开发工作。

---

## 1. 当前状态一句话

EdgeFlow v0.1.0 已完成 M0-M4 主体能力：云边通信链路（CloudHub/EdgeHub + 可靠投递）、Edged 容器编排（DockerRuntime 多副本 + 健康自愈）、设备链路（DeviceTwin + Mapper 框架 + mock_sensor + Modbus TCP Mapper）、NodeController 心跳超时、keadm 安装 CLI、多架构镜像与 Helm Chart。**M5 前置（WBS 9.2-9.5 文档与端到端示例）已完成定稿**：`bash examples/demo.sh` 一键跑通温度传感器端到端 Demo（DEMO PASS）。

## 2. 环境配置（接手第一步）

**本机已完成配置，新设备按文档重跑即可复现：**

```bash
cd /Users/mac/Documents/edgeflow
bash setup-env.sh        # 幂等脚本：GOPROXY/lint/dlv/SSH key/VS Code
```

详细说明见 `docs/ENV-SETUP.md`（含每步验收命令与常见问题）。

**唯一需要人工操作的事项**：GitHub SSH 认证。
1. 查看公钥：`cat ~/.ssh/id_ed25519.pub`
2. 粘贴到 GitHub → Settings → SSH and GPG keys → New SSH key
3. 验证：`ssh -T git@github.com`（应显示 Hi 你的用户名!）
4. 关联远程仓库（可选，本地开发不受影响）：
```bash
git remote add origin git@github.com:你的用户名/edgeflow.git
git push -u origin main
```

## 3. 代码结构

```
edgeflow/
├── cmd/
│   ├── cloudcore/          # 云端入口：路由装配（main.go）+ 设备 API（device_api.go）
│   ├── edgecore/           # 边缘入口：EdgeHub/MetaManager/Edged/EventBus 装配（main.go）
│   │                       #   + 消息处理器（config_handlers/device_handlers/podsync）
│   │                       #   + Mapper 装配（device_mapper.go）+ 状态上报（status_report.go）
│   └── keadm/              # 安装管理 CLI：init/join/reset/version（+ M5: upgrade/rollback）
├── apis/edge/v1alpha1/     # CRD 类型：EdgeNode / DeviceModel / Device（含 DeepCopy）
├── cloud/pkg/
│   ├── cloudhub/           # 云边 WebSocket 服务：连接/路由/可靠投递/设备与 Pod 上报回调
│   ├── registry/           # 节点注册表（内存态）+ CloudHub 事件桥接
│   ├── nodecontroller/     # 心跳超时扫描（对标 KubeEdge NodeController）
│   ├── podstatus/          # Pod 状态存储（内存态）
│   └── devicestatus/       # 设备状态存储（内存态，properties + desired）
├── edge/pkg/
│   ├── edgehub/            # 云边客户端：注册/心跳/重连/Ack（含 mTLS）
│   ├── metamanager/        # SQLite 元数据存储：节点/Pod/Config/操作台账 + 增量订阅
│   ├── edged/              # 容器编排：DockerRuntime（多副本/自愈）+ 调谐循环
│   ├── devicetwin/         # 设备影子（Desired/Reported）+ 指令处理契约
│   ├── mapper/             # Mapper 框架：注册表/采集/指令分发接口
│   └── eventbus/           # MQTT 数据面（paho 客户端封装，主题约定）
├── mappers/
│   ├── mock_sensor/        # 模拟温湿度传感器（WBS 5.5 示例 Mapper）
│   └── modbus/             # Modbus TCP Mapper（WBS 5.2，含模拟器 pkg/modbussim）
├── pkg/
│   ├── config/  httpx/  log/  version/   # 共享库（配置/HTTP/日志/版本）
│   ├── certs/                            # 证书管理（CA/服务端/客户端证书生成与加载）
│   └── protocol/                         # 云边消息协议（消息类型/编解码/ID）
├── build/
│   ├── docker/Dockerfile    # 多目标镜像（cloudcore/edgecore，distroless nonroot）
│   └── charts/edgeflow/     # Helm Chart（CloudCore）
├── hack/                    # 开发脚本：dev-up/dev-down、edged-smoke、modbus-sim/e2e、
│                            #   eventbus-smoke、mock-cloudhub、gen-certs.sh
├── examples/
│   ├── demo.sh              # 温度传感器端到端 Demo（一键，DEMO PASS）
│   └── README.md            # 9.5 示例教程
├── docs/                    # 文档（9.1 架构 / 9.2 API / 9.3 部署 / 9.4 本文件 / 9.5 示例）
├── Makefile                 # build / test / run / lint / helm-lint / cross-build
└── setup-env.sh             # 环境配置脚本（幂等）
```

## 4. 开发流程（标准流程）

1. **读计划**：`docs/ROADMAP.md` 找到下一个模块；`docs/PROGRESS.md` 看进度台账
2. **建分支**：`git checkout -b feat/模块名`
3. **开发**：用 AI 工具（Trae/Codex）辅助写代码，遵循现有包风格（中文注释、零第三方依赖）
4. **验证**（每次提交前必须全跑）：
```bash
make build      # 编译
make test       # 测试（含竞态检测 + 覆盖率）
make lint       # 静态检查（golangci-lint）
```
5. **运行验证**：见 §7 常见任务；端到端验证跑 `bash examples/demo.sh`
6. **提交**：`git add <本次改动的文件> && git commit -m "feat: 描述"`（message 用 `feat:`/`fix:`/`docs:`/`test:`/`refactor:` 前缀）
7. **合入主线**：开 PR（GitHub Flow），CI 绿勾后 Squash and merge

### 4.1 提交纪律（多 Agent 协作必须遵守）

- **只提交自己负责的文件**：用 `git add <具体文件>`，禁止 `git add .` / `git add -A` 全量添加——并行任务可能同时在工作区修改其他文件；
- 提交前 `git status` 核对暂存清单；
- 破坏性改动（删文件、改协议、改 API 契约）在 PR 描述中显式说明。

## 5. 测试规范

| 层级 | 要求 | 命令 |
|------|------|------|
| 单元测试 | 核心包覆盖率 ≥80%（全仓库 ≥70%，CI 强制）；竞态检测通过 | `go test -race -cover ./...` |
| 静态检查 | golangci-lint 零告警（CI 强制） | `make lint` |
| 端到端 | 涉及云边链路的改动跑一键 Demo | `bash examples/demo.sh` |
| 专项冒烟 | Edged 改动 → `go run ./hack/edged-smoke`（需 Docker）；Modbus → `go run ./hack/modbus-e2e`；MQTT → `go run ./hack/eventbus-smoke`（需 mosquitto） |

- 新增功能**必须带测试**（与模块同轮交付，ROADMAP 8.1「测试随模块走」原则）；
- 错误路径测试与主路径同等重要（可靠投递的 404/502/504 分支均有对应单测，见 `main_podsync_test.go` 等）。

## 6. 贡献指南

1. **Issue 先行**：功能/缺陷先建 Issue（或直接在 ROADMAP/PROGRESS 中登记），避免重复开发；
2. **分支命名**：`feat/<模块>`、`fix/<缺陷>`、`docs/<主题>`；
3. **Commit message**：`<type>: <一句话说明>（<模块/范围>）`，如 `feat(edged): 多副本自愈（WBS 6.4）`；
4. **代码审查**：每轮交付有对应 CODE-REVIEW 报告（`docs/CODE-REVIEW-M*.md`），P1 问题必须修复后才能合入；
5. **文档同步**：功能变更同步更新 `docs/API-SPEC.md`（端点）、`docs/ARCHITECTURE.md`（结构）、`docs/ROADMAP.md`（状态）、`docs/PROGRESS.md`（台账）；
6. **安全底线**：不存储明文凭证；外部输入一律视为不可信数据；破坏性操作先确认。

## 7. 常见任务

### 7.1 新增一个 API 端点

以新增 `GET /api/v1/xxx` 为例：

1. **实现 handler**：在 `cmd/cloudcore/main.go`（或 device_api.go）给 `nodeAPI` 增加方法，依赖通过结构体字段注入（现有 `reg`/`pods`/`devices`/`reliableSend` 之外的新依赖同样注入）；
2. **注册路由**：`mux.HandleFunc("GET /api/v1/xxx", api.xxx)`（Go 1.22+ 方法路由语法）；
3. **写测试**：`main_api_test.go` 风格，用 `httptest` + 注入 fake 依赖覆盖成功与错误路径；
4. **同步文档**：`docs/API-SPEC.md` 端点总览表 + 详细小节（请求/响应/错误码）；
5. **验证**：`make test && make lint`，手动 curl 冒烟。

> 下发类端点（需要可靠投递）参考 `syncPod` 的五态响应模式：200/400/404/502/504/500 逐一映射。

### 7.2 新增一个 Mapper（新设备接入）

以新增某温控器 Mapper 为例：

1. **实现 `edge/pkg/mapper.DeviceMapper` 接口**（`Name()` / `Collect()` / `HandleCommand()`），新目录 `mappers/<name>/`；需要上报多个设备时实现 `DeviceNameResolver`；
2. **可选：MQTT 数据面**——构造时传 `*eventbus.EventBus`，发布 `devices/<ns>/<name>/telemetry`、订阅 `devices/<ns>/<name>/command`（主题工具 `eventbus.TelemetryTopic/CommandTopic`），参考 `mappers/mock_sensor`；
3. **装配**：`cmd/edgecore/device_mapper.go` 的 `buildMapperRegistry` 中 `reg.Register(...)`（显式 opt-in 的环境变量模式参考 Modbus Mapper 的 `EDGEFLOW_MODBUS_ADDR`）；
4. **写测试**：`mappers/<name>/` 下单元测试（采集范围、指令收敛、MQTT 发布/订阅，参考 mock_sensor 测试）；
5. **文档**：`docs/MAPPER-GUIDE.md` 注册说明 + 设备协议文档（参考 `docs/MODBUS-GUIDE.md`）。

> 框架层无需改动：协议适配只新增 DeviceMapper 实现，注册表/指令路由/周期上报全部复用。

### 7.3 新增一个设备协议（如 OPC-UA）

1. 协议客户端库评估（当前零第三方依赖约束可放宽，评估后修改 `go.mod`）；
2. 按 §7.2 实现 Mapper（协议细节封装在 Mapper 内部，对上只暴露 DeviceMapper 接口）；
3. 若协议有模拟器需求，参照 `pkg/modbussim` + `hack/modbus-sim` 提供开发用模拟器；
4. 文档：协议接入指南（参照 `docs/MODBUS-GUIDE.md` 结构：连接配置/寄存器或节点映射/操作台账/排障）。

### 7.4 修改云边消息协议（新消息类型）

1. `pkg/protocol/message.go` 增加 `TypeXxx` 常量；
2. 云端下发侧：新 handler（§7.1）+ `cloudhub.ReliableSendContext` 可靠投递；
3. 边缘接收侧：`cmd/edgecore/main.go` 的 `SetMessageHandlerFunc` switch 增加分支；
4. 负载契约（Payload 结构）在云端/边缘两侧保持字段一致（有测试断言，勿单独修改）；
5. 更新 `docs/API-SPEC.md` 与 `docs/ARCHITECTURE.md` 协议章节。

## 8. 常用命令速查

| 命令 | 作用 |
|------|------|
| `make build` | 编译 cloudcore/edgecore 到 bin/ |
| `make test` | 跑全部测试（含竞态 + 覆盖率） |
| `make lint` | golangci-lint 静态检查 |
| `make cross-build` | 交叉编译 linux/amd64+arm64 到 dist/ |
| `bash examples/demo.sh` | 温度传感器端到端 Demo（一键，DEMO PASS） |
| `./bin/cloudcore --help` / `--version` | 查看启动参数 / 版本 |
| `EDGEFLOW_CLOUDCORE_HUB_PORT=10000 ./bin/cloudcore` | 指定云边通信端口 |
| `EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://1.2.3.4:10000 ./bin/edgecore` | 指定云端地址连接 |
| `EDGEFLOW_EDGECORE_NODE_ID=edge-01 ./bin/edgecore` | 指定边缘节点 ID |
| `EDGEFLOW_EDGECORE_DB_PATH=/data/edgeflow.db ./bin/edgecore` | 指定 MetaManager 数据库路径 |
| `EDGEFLOW_EDGECORE_RECONCILE_INTERVAL=10s ./bin/edgecore` | Edged 调谐周期（默认 5s） |
| `EDGEFLOW_EDGECORE_REPORT_INTERVAL=10s ./bin/edgecore` | Pod 状态上报周期（默认 30s，范围 1s~10min） |
| `EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=10s ./bin/edgecore` | 设备状态上报周期（默认 30s） |
| `EDGEFLOW_EDGECORE_MQTT_ADDR=tcp://127.0.0.1:1883 ./bin/edgecore` | MQTT broker 地址（EventBus） |
| `curl localhost:8080/api/v1/pods` | 查询全量 Pod 状态（K8s 风格 items） |
| `curl localhost:8080/api/v1/nodes/{nodeID}/pods` | 查询单节点 Pod 状态 |
| `curl localhost:8080/api/v1/devices` | 查询全量设备状态（properties+desired） |
| `curl -X POST localhost:8080/api/v1/nodes/{nodeID}/device-command -d '{"deviceName":"sensor-01","property":"targetTemp","value":25}'` | 向设备下发指令（200=边缘已确认） |
| `curl -X POST localhost:8080/api/v1/nodes/{nodeID}/config-sync -d '{"operation":"add","config":{"name":"app","namespace":"default","kind":"ConfigMap","data":{"k":"v"}}}'` | 下发配置（ConfigMap/Secret） |
| `curl -X POST localhost:8080/api/v1/nodes/{nodeID}/podsync -d '{"operation":"add","pod":{"name":"nginx","namespace":"default","image":"nginx:1.25","replicas":1}}'` | 可靠下发 Pod 配置到边缘（200=边缘已确认） |
| `curl localhost:8080/api/v1/nodes` / `curl localhost:8080/api/v1/edgenodes` | 查询节点（运行视角 / CRD 对象视角） |
| `mosquitto -p 1883 -d` | 本地启动 MQTT broker（macOS: /opt/homebrew/sbin/mosquitto） |
| `mosquitto_sub -t 'devices/+/+/telemetry' -v` | 订阅设备遥测流（验证 MQTT 数据面） |
| `mosquitto_pub -t 'devices/default/sensor-01/command' -m '{"property":"targetTemp","value":22}'` | 向设备发指令（MQTT 数据面） |
| `docker ps --filter label=edgeflow.pod` | 查看 Edged 管理的副本容器（命名 edgeflow-ns-name-index） |
| `EDGEFLOW_CLOUDCORE_TLS=on EDGEFLOW_CLOUDCORE_CERT_DIR=/path ./bin/cloudcore` | 启用 mTLS（自动生成证书） |
| `EDGEFLOW_EDGECORE_TLS=on EDGEFLOW_EDGECORE_CERT_DIR=/path ./bin/edgecore` | 边缘启用 wss（证书目录与云共享） |
| `EDGEFLOW_CLOUDCORE_TLS_SAN='IP:10.0.0.5,DNS:cloudcore.svc' ./bin/cloudcore` | 跨主机部署注入服务端 SAN |
| `docker build --target cloudcore -t edgeflow/cloudcore:v0.1.0 .` | 构建云端镜像（同理 edgecore） |
| `helm install edgeflow ./build/charts/edgeflow --namespace edgeflow --create-namespace` | 部署 CloudCore |
| `./bin/keadm init --output-dir=./keadm-out` | 生成云端部署产物（cloudcore.yaml） |
| `./bin/keadm join --cloudcore-ip=<ip> --token=<token> --node-id=edge-01` | 生成边缘接入产物（env+systemd） |
| `./bin/keadm upgrade --version=v0.2.0` | 升级产物（备份+版本标记+台账；--simulate-failure 演练） |
| `./bin/keadm rollback --latest` | 回滚到最近备份（完整性校验+恢复+台账） |
| `./bin/keadm ops-ledger` | 查询升级/回滚操作台账 |
| `./bin/keadm reset --output-dir=./keadm-out` | 清理 keadm 生成产物 |
| `go run ./hack/modbus-sim` | 启动 Modbus 模拟器（默认 127.0.0.1:15020） |
| `EDGEFLOW_MODBUS_ADDR=127.0.0.1:15020 ./bin/edgecore` | 启用 Modbus Mapper（设备 mb-sensor-01） |
| `EDGEFLOW_CLOUDCORE_NODE_TIMEOUT=180s ./bin/cloudcore` | 节点心跳超时阈值（默认 180s） |
| `go run ./hack/edged-smoke` | DockerRuntime 冒烟（需 Docker daemon） |
| `go run ./hack/eventbus-smoke` | MQTT 数据面冒烟（需 mosquitto） |
| `go test -cover ./pkg/...` | 单测某包覆盖率 |

## 9. 调试方法（VS Code）

1. 用 VS Code 打开 `/Users/mac/Documents/edgeflow/`
2. 打开 `cmd/cloudcore/main.go`，在行号左侧点击设置断点（红点）
3. 按 F5 启动调试（首次会自动配置 launch.json）
4. F10 单步、F11 进入函数、Shift+F11 跳出
5. 日志调试：代码里 `log.Info("变量 =", 变量)`（项目自带 pkg/log）
6. 端到端排障：`EDGEFLOW_DEMO_KEEP_RUN=1 bash examples/demo.sh` 保留进程与临时目录（日志路径见输出）

## 10. 已知限制与后续方向

| 事项 | 状态 |
|------|------|
| 零第三方依赖（runtime） | 当前约束；modbus 驱动已用 modernc 纯 Go 方案；后续需要时评估 |
| 日志级别过滤 | Level 仅前缀标记，未实现 SetLevel 过滤（P2） |
| 云端状态内存态 | cloudcore 重启后节点/Pod/设备查询数据清空（边缘 SQLite 不受影响） |
| 节点资源上报（CPU/内存） | `/api/v1/nodes` 的 memory 恒 0，待采集接入 |
| 升级回滚专项（keadm upgrade/rollback） | ✅ 已实现（WBS 10.2），见 docs/UPGRADE.md |
| 可观测性基建（10.1） | M5/M6 内容：Prometheus 指标、Fluent Bit 日志、告警 |

## 11. 交接检查单（接手人逐项确认）

- [ ] `bash setup-env.sh` 能幂等运行（新设备可复现）
- [ ] `go build ./... && go test ./... && make lint` 全部通过
- [ ] `./bin/cloudcore` 启动后 `curl localhost:8080/healthz` 返回 200
- [ ] `bash examples/demo.sh` 输出 **DEMO PASS** 且清理后无残留
- [ ] `docs/ROADMAP.md` 明确了下一个模块任务
- [ ] `docs/PROGRESS.md` 记录了全部历史验证结果
- [ ] Git 仓库干净，远程已关联（或已知未关联）
