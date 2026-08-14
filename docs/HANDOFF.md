# EdgeFlow 交接说明文档（HANDOFF）

> 目的：让后续开发者（包括零基础用户本人）能按本文档独立接过开发工作。
> 最后更新：2026-08-13 09:25 (Asia/Shanghai)

---

## 1. 当前状态一句话

环境已配置完成（可复现）、Git 仓库已建立（7 个提交）、首个模块（共享库 + cloudcore healthz 服务）已完成并通过全部验证（总覆盖率 87.5%），代码审查的 3 项 P1 条件已全部修复。

## 2. 环境配置（接手第一步）

**本机已完成配置，新设备按文档重跑即可复现：**

```bash
cd /Users/mac/Documents/edgeflow
bash setup-env.sh        # 幂等脚本：GOPROXY/lint/dlv/SSH key/VS Code
```

详细说明见 `docs/ENV-SETUP.md`（168 行，含每步验收命令与常见问题）。

**唯一需要人工操作的事项**：GitHub SSH 认证。
1. 查看公钥：`cat ~/.ssh/id_ed25519.pub`
2. 粘贴到 GitHub → Settings → SSH and GPG keys → New SSH key
3. 验证：`ssh -T git@github.com`（应显示 Hi 你的用户名!）
4. 关联远程仓库（可选，本地开发不受影响）：
```bash
git remote add origin git@github.com:你的用户名/edgeflow.git
git push -u origin main
```

## 3. 项目结构

```
edgeflow/
├── cmd/
│   ├── cloudcore/        # 云端入口（已实现：healthz 健康检查 + 配置加载 + 优雅退出）
│   └── edgecore/         # 边缘端入口（占位，M1 开发）
├── pkg/
│   ├── config/           # 配置库（JSON 配置文件，优先级：--port > 环境变量 > 文件 > 默认）
│   ├── httpx/            # HTTP 工具（healthz handler）
│   ├── log/              # 日志封装（INFO/WARN/ERROR + 时间戳）
│   └── version/          # 版本信息（构建时注入）
├── config/
│   └── cloudcore.json    # cloudcore 示例配置（{"port": 8080}）
├── docs/
│   ├── ROADMAP.md        # 模块任务拆解与完成标准（开发主文档）
│   ├── ENV-SETUP.md      # 环境配置说明
│   ├── CODE-REVIEW.md    # 首个模块代码审查报告
│   └── PROGRESS.md       # 项目进度台账
├── .github/workflows/ci.yml  # CI：lint（强制）+ build + test + 覆盖率≥70%
├── Makefile              # make build / test / run / lint
├── setup-env.sh          # 环境配置脚本（幂等）
└── README.md             # 快速开始
```

## 4. 如何开发下一个模块（标准流程）

1. **读计划**：`docs/ROADMAP.md` 找到下一个模块（当前建议 M4 主体：keadm 基础版（8.6）、NodeController 心跳监控（2.4）、或真实协议 Mapper（5.2 Modbus））
2. **建分支**：`git checkout -b feat/模块名`
3. **开发**：用 AI 工具（Trae/Codex）辅助写代码，遵循现有包风格（中文注释、零第三方依赖）
4. **验证**（每次提交前必须全跑）：
```bash
make build      # 编译
make test       # 测试（含覆盖率）
make lint       # 静态检查
```
5. **运行验证**：`go run ./cmd/cloudcore` 启动后 `curl http://localhost:8080/healthz`
6. **提交**：`git add . && git commit -m "feat: 描述"`（message 用 feat:/fix:/docs: 前缀）
7. **合入主线**：开 PR（GitHub Flow），CI 绿勾后 Squash and merge

## 5. 常用命令速查

| 命令 | 作用 |
|------|------|
| `make build` | 编译 cloudcore/edgecore 到 bin/ |
| `make run` | 运行 cloudcore |
| `make test` | 跑全部测试（显示覆盖率） |
| `make lint` | golangci-lint 静态检查 |
| `./bin/cloudcore --help` | 查看启动参数 |
| `./bin/cloudcore --version` | 查看版本 |
| `EDGEFLOW_CLOUDCORE_HUB_PORT=10000 ./bin/cloudcore` | 指定云边通信端口 |
| `EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://1.2.3.4:10000 ./bin/edgecore` | 指定云端地址连接 |
| `EDGEFLOW_EDGECORE_NODE_ID=edge-01 ./bin/edgecore` | 指定边缘节点 ID |
| `EDGEFLOW_EDGECORE_DB_PATH=/data/edgeflow.db ./bin/edgecore` | 指定 MetaManager 数据库路径 |
| `EDGEFLOW_EDGECORE_RECONCILE_INTERVAL=10s ./bin/edgecore` | Edged 调谐周期（默认 5s） |
| `EDGEFLOW_EDGECORE_REPORT_INTERVAL=10s ./bin/edgecore` | Pod 状态上报周期（默认 30s，范围 1s~10min） |
| `curl localhost:8080/api/v1/pods` | 查询全量 Pod 状态（K8s 风格 items） |
| `curl localhost:8080/api/v1/nodes/{nodeID}/pods` | 查询单节点 Pod 状态 |
| `curl localhost:8080/api/v1/devices` | 查询全量设备状态（properties+desired） |
| `curl -X POST localhost:8080/api/v1/nodes/{nodeID}/device-command -d '{"deviceName":"sensor-01","property":"targetTemp","value":25}'` | 向设备下发指令（200=边缘已确认） |
| `EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=10s ./bin/edgecore` | 设备状态上报周期（默认 30s） |
| `curl -X POST localhost:8080/api/v1/nodes/{nodeID}/config-sync -d '{"operation":"add","config":{"name":"app","namespace":"default","kind":"ConfigMap","data":{"k":"v"}}}'` | 下发配置（ConfigMap/Secret） |
| `EDGEFLOW_EDGECORE_MQTT_ADDR=tcp://127.0.0.1:1883 ./bin/edgecore` | MQTT broker 地址（EventBus，预留装配） |
| `mosquitto -p 1883 -d` | 本地启动 MQTT broker（测试/开发用；路径 /opt/homebrew/sbin/mosquitto） |
| `mosquitto_sub -t 'devices/+/+/telemetry' -v` | 订阅设备遥测流（验证 MQTT 数据面） |
| `mosquitto_pub -t 'devices/default/sensor-01/command' -m '{"property":"targetTemp","value":22}'` | 向设备发指令（MQTT 数据面） |
| `docker ps --filter label=edgeflow.pod` | 查看 Edged 管理的副本容器（命名 edgeflow-ns-name-index） |
| `EDGEFLOW_CLOUDCORE_TLS=on EDGEFLOW_CLOUDCORE_CERT_DIR=/path ./bin/cloudcore` | 启用 mTLS（自动生成证书） |
| `EDGEFLOW_EDGECORE_TLS=on EDGEFLOW_EDGECORE_CERT_DIR=/path ./bin/edgecore` | 边缘启用 wss（证书目录与云共享） |
| `EDGEFLOW_CLOUDCORE_TLS_SAN='IP:10.0.0.5,DNS:cloudcore.svc' ./bin/cloudcore` | 跨主机部署注入服务端 SAN |
| `docker build --target cloudcore -t edgeflow/cloudcore:v0.1.0 .` | 构建云端镜像（同理 edgecore） |
| `helm install edgeflow ./build/charts/edgeflow --namespace edgeflow --create-namespace` | 部署 CloudCore（含 /data 可写挂载） |
| `go run ./hack/edged-smoke` | DockerRuntime 冒烟（需 Docker daemon） |
| `docker ps --filter label=edgeflow.pod` | 查看 Edged 管理的容器 |
| `curl localhost:8080/api/v1/nodes` | 查询已注册边缘节点（JSON） |
| `curl localhost:8080/api/v1/edgenodes` | 查询 EdgeNode CRD 对象（K8s 风格 items） |
| `curl -X POST localhost:8080/api/v1/nodes/{nodeID}/podsync -d '{"operation":"add","pod":{"name":"nginx","namespace":"default","image":"nginx:1.25","replicas":1}}'` | 可靠下发 Pod 配置到边缘（200=边缘已确认） |
| `EDGEFLOW_CLOUDCORE_PORT=10000 ./bin/cloudcore` | 环境变量指定端口 |
| `./bin/cloudcore --port 7070` | 命令行指定端口 |
| `go test -cover ./pkg/...` | 单测某包覆盖率 |

## 6. 调试方法（VS Code）

1. 用 VS Code 打开 `/Users/mac/Documents/edgeflow/`
2. 打开 `cmd/cloudcore/main.go`，在行号左侧点击设置断点（红点）
3. 按 F5 启动调试（首次会自动配置 launch.json）
4. F10 单步、F11 进入函数、Shift+F11 跳出
5. 日志调试：代码里 `log.Info("变量 =", 变量)`（项目自带 pkg/log）

## 7. 已知限制与后续方向

| 事项 | 状态 |
|------|------|
| 零第三方依赖 | 当前设计约束，后续需要时评估（如 NATS 客户端、yaml 解析） |
| 日志级别过滤 | Level 仅前缀标记，未实现 SetLevel 过滤（P2） |
| edgecore | 占位程序，M1 开发 |
| 云边通信（WebSocket） | ✅ M1 完成：协议+注册+心跳+重连+可靠投递+PodSync 链路 |
| Edged（3.2） | 🟨 方案 A POC 通过：DockerRuntime 驱动真实容器（方案 A 定案，M2 继续） |
| 设备管理（CRD） | M0-2/M3 内容，未开始 |
| Docker/Kubernetes 本地环境 | 本机 Docker 已装；kind 环境脚本见 docs/DEV-ENV.md（hack/dev-up.sh） |

## 8. 交接检查单（接手人逐项确认）

- [ ] `bash setup-env.sh` 能幂等运行（新设备可复现）
- [ ] `go build ./... && go test ./... && make lint` 全部通过
- [ ] `./bin/cloudcore` 启动后 `curl localhost:8080/healthz` 返回 200
- [ ] `docs/ROADMAP.md` 明确了下一个模块任务
- [ ] `docs/PROGRESS.md` 记录了全部历史验证结果
- [ ] Git 仓库干净，远程已关联（或已知未关联）
