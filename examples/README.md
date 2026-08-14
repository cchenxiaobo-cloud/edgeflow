# 温度传感器端到端 Demo（WBS 9.5 示例教程）

> 对应 ROADMAP WBS 9.5「示例与教程」。本教程对标 KubeEdge 官方温度传感器示例（云端下发 Pod + 设备数据流 + MQTT 数据面），
> 在 EdgeFlow 上用**模拟温湿度传感器（mock_sensor）**完整演示云边协同链路。
> 状态：✅ **v0.1.0 定稿**（2026-08-14），`examples/demo.sh` 已实际跑通（DEMO PASS）。

## 1. 演示什么

一次运行完整覆盖 EdgeFlow 的三条核心链路：

| 链路 | 内容 | 验证点 |
|------|------|--------|
| 云边通道 | edgecore 注册到 cloudcore（WebSocket，心跳保活） | `GET /api/v1/nodes` 出现 `Ready` 节点 |
| 应用下发 | 云端下发 nginx Pod → Edged 调谐创建容器（DockerRuntime）→ 状态回传 | `docker ps` 见 `edgeflow-default-<pod>-0`；`GET /api/v1/pods` 见 `Running` |
| 设备数据流 | mock_sensor 每 2s 采集温湿度 → 设备影子 → 周期上报云端；云端下发指令 → Mapper 执行 | `GET /api/v1/devices` 见 sensor-01；`device-command` 后 `desired` 写入 |
| MQTT 数据面（可选） | 检测到 mosquitto 时：遥测发布 `devices/+/+/telemetry`、指令订阅 `devices/+/+/command` | `mosquitto_sub` 收到遥测；`mosquitto_pub` 指令生效 |

## 2. 架构

```
┌─────────────────────────── 云端（本机） ───────────────────────────┐
│  cloudcore                                                         │
│   ├─ HTTP :8080     /healthz /api/v1/nodes /api/v1/pods            │
│   │                /api/v1/devices /podsync /device-command ...    │
│   └─ CloudHub :10000（WebSocket /v1/edge）                          │
└──────────────────────────────┬─────────────────────────────────────┘
                               │ 注册 / 心跳 / 可靠投递（Ack）
┌──────────────────────────────┴─────────────────────────────────────┐
│  edgecore（边缘）                                                    │
│   ├─ EdgeHub        云边通信客户端（断线重连 + mTLS 可选）             │
│   ├─ MetaManager    SQLite 元数据（节点/Pod/Config，本地持久化）        │
│   ├─ Edged          DockerRuntime：调谐创建/自愈容器                   │
│   │                    └── docker run edgeflow-default-<pod>-0       │
│   ├─ DeviceTwin     设备影子（desired/reported）                      │
│   ├─ Mapper         mock_sensor（sensor-01：temperature/humidity）   │
│   │                    └── 采集循环每 2s 波动一次                      │
│   └─ EventBus（可选） MQTT 数据面 ──► mosquitto :1883                 │
└─────────────────────────────────────────────────────────────────────┘
                              ▲
                MQTT（可选）   │  遥测 devices/default/sensor-01/telemetry
                              │  指令 devices/default/sensor-01/command
                        ┌─────┴─────┐
                        │ mosquitto │（本机 broker，可选）
                        └───────────┘
```

## 3. 前置条件

| 依赖 | 版本 | 说明 |
|------|------|------|
| Go | 1.26+ | 构建二进制 |
| Make | 任意 | `make build` |
| Docker | daemon 运行中 | Edged 容器运行时；nginx:1.25-alpine 镜像建议本地缓存（首次会自动拉取） |
| curl | 任意 | 调用 cloudcore API |
| mosquitto | 可选 | MQTT 数据面。macOS: `brew install mosquitto`（broker 在 `/opt/homebrew/sbin/mosquitto`） |
| python3 | 可选（推荐） | 用于空闲端口探测与 JSON 美化输出（缺失时自动回退） |

> 无需 Kubernetes 集群、无需联网（镜像已缓存时）、无需 root 权限。

## 4. 快速开始（一键脚本）

```bash
cd edgeflow
bash examples/demo.sh
```

脚本自动执行完整流程（构建 → 启动 → 验证 → 清理），约 40 秒，最终输出：

```
==> 11/11 清理演示资源（podsync delete → 容器回收）
{"status":"ok","acked":true}
[demo] 容器已回收: edgeflow-default-demo-nginx-...-0

========================================
  DEMO PASS ✅（2026-08-14 19:11:58）
========================================
```

脚本特性：

- **幂等**：每次运行随机 node-id / 端口 / Pod 名，重复运行互不冲突；
- **失败即退出**：`set -euo pipefail`，任一步失败输出 DEMO FAIL 与日志路径；
- **自动清理**：进程（SIGTERM 优雅退出）、容器、临时目录（SQLite/日志）全部回收；
- **MQTT 可选段**：检测到 mosquitto broker 才执行数据面验证，否则跳过并说明。

常用环境变量（均有默认值，一般无需设置）：

| 变量 | 作用 |
|------|------|
| `EDGEFLOW_DEMO_SKIP_BUILD=1` | 跳过 `make build`（复用已有 bin/） |
| `EDGEFLOW_DEMO_HTTP_PORT` / `EDGEFLOW_DEMO_HUB_PORT` / `EDGEFLOW_DEMO_MQTT_PORT` | 固定端口（默认随机空闲端口） |
| `EDGEFLOW_DEMO_POD_IMAGE` | 下发 Pod 的镜像（默认 nginx:1.25-alpine） |
| `EDGEFLOW_DEMO_KEEP_RUN=1` | 结束时保留进程与临时目录（排障用） |

## 5. 分步教程（手动执行）

以下步骤演示脚本内部的每个环节，便于理解链路与二次开发。

### 5.1 构建

```bash
cd edgeflow
make build
# 产物: bin/cloudcore, bin/edgecore
```

### 5.2 启动 cloudcore（终端 A）

```bash
./bin/cloudcore
# 预期日志:
#   cloudcore starting, version=v0.1.0 ...
#   HTTP server listening on :8080
#   CloudHub ... listening on :10000
```

### 5.3 启动 edgecore（终端 B）

```bash
EDGEFLOW_EDGECORE_NODE_ID=edge-node-1 ./bin/edgecore
# 预期日志:
#   MetaManager opened: data/edgeflow.db
#   EdgeHub connecting to ws://127.0.0.1:10000 as edge-node-1
#   MockSensor sensor-01 启动（interval=2s）
#   Edged started（方案 A POC：DockerRuntime + 5s 调谐周期）
#   MetaManager 已保存节点注册信息（nodeID=edge-node-1, ...）
```

> 未安装 mosquitto 时会看到 EventBus 连接失败告警并降级为纯本地模式——属预期行为，云边链路不受影响。

### 5.4 验证节点注册

```bash
curl http://127.0.0.1:8080/api/v1/nodes
```

预期输出（节选）：

```json
[{"nodeID":"edge-node-1","nodeName":"edge-node-1","arch":"arm64","os":"darwin",
  "edgecoreVersion":"version=v0.1.0 ...","cpu":8,"ip":"127.0.0.1",
  "registeredAt":1786705914423,"lastHeartbeatAt":1786705914423,"status":"Ready"}]
```

### 5.5 下发 Pod（nginx）

```bash
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-node-1/podsync \
  -H 'Content-Type: application/json' \
  -d '{"operation":"add","pod":{"name":"nginx","namespace":"default","image":"nginx:1.25-alpine","replicas":1}}'
# 预期: {"status":"ok","acked":true}   ← 边缘已确认（可靠投递）
```

### 5.6 查看容器

```bash
docker ps --filter label=edgeflow.pod
# 预期: edgeflow-default-nginx-0  Up ...   （命名规范 edgeflow-<ns>-<name>-<index>）
```

### 5.7 查看 Pod 状态（云端视角）

```bash
curl http://127.0.0.1:8080/api/v1/pods
# 预期: items[0].phase == "Running"（状态上报周期默认 30s，可设
#       EDGEFLOW_EDGECORE_REPORT_INTERVAL=3s 加速观察）
```

### 5.8 设备数据流

```bash
curl http://127.0.0.1:8080/api/v1/devices
# 预期: items[0].deviceName == "sensor-01"，properties 含 temperature/humidity
```

设备指令（设置目标温度 25℃）：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-node-1/device-command \
  -H 'Content-Type: application/json' \
  -d '{"deviceName":"sensor-01","property":"targetTemp","value":25}'
# 预期: {"status":"ok","acked":true}
curl http://127.0.0.1:8080/api/v1/devices
# 预期: desired: {"targetTemp":25}（云端期望值已写入）
```

### 5.9 MQTT 数据面（可选）

前置：`brew install mosquitto`，然后：

```bash
mosquitto -p 1883 -d                      # 启动 broker（macOS: /opt/homebrew/sbin/mosquitto）
# 重启 edgecore 并指定 broker 地址（MQTT 模式在启动时装配）
EDGEFLOW_EDGECORE_MQTT_ADDR=tcp://127.0.0.1:1883 \
EDGEFLOW_EDGECORE_NODE_ID=edge-node-1 ./bin/edgecore
```

验证遥测流（传感器每 2s 发布一次温湿度）：

```bash
mosquitto_sub -t 'devices/+/+/telemetry' -v -C 2
# 预期:
# devices/default/sensor-01/telemetry {"temperature":28.74,"humidity":46.94,"ts":1786705918423}
# devices/default/sensor-01/telemetry {"temperature":29.11,"humidity":47.20,"ts":1786705920423}
```

验证指令通道：

```bash
mosquitto_pub -t 'devices/default/sensor-01/command' \
  -m '{"property":"targetTemp","value":23}'
# 预期（edgecore 日志）: MockSensor sensor-01: 数据面指令生效 targetTemp=23
```

### 5.10 清理

```bash
# 1. 回收 Pod（Edged 自动删除容器）
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-node-1/podsync \
  -d '{"operation":"delete","pod":{"name":"nginx","namespace":"default"}}'
# {"status":"ok","acked":true}

# 2. 停止进程（Ctrl+C 或 kill -TERM，均支持优雅退出）
# 3. 兜底清理（如上次运行异常残留）
docker rm -f $(docker ps -aq --filter label=edgeflow.pod) 2>/dev/null
rm -f data/edgeflow.db
```

## 6. 预期输出总览（demo.sh 实测，2026-08-14）

```
==> 4/11 验证节点注册（GET /api/v1/nodes）
[{... "nodeID":"demo-node-1786705910-7810", "status":"Ready"}]
==> 5/11 下发 Pod（podsync: demo-nginx-... / nginx:1.25-alpine / replicas=1）
{"status":"ok","acked":true}
==> 6/11 验证容器运行（docker ps edgeflow-*）
edgeflow-default-demo-nginx-...-0   Up
==> 7/11 验证 Pod 状态上报（GET /api/v1/pods → Running）
{"kind":"PodStatusList","apiVersion":"v1","items":[{"podName":"demo-nginx-...","phase":"Running"}]}
==> 8/11 验证设备数据流（GET /api/v1/devices → sensor-01）
{"kind":"DeviceStatusList","apiVersion":"v1","items":[{"deviceName":"sensor-01",
  "properties":{"humidity":46.39,"temperature":29.38}}]}
==> 9/11 下发设备指令（device-command: targetTemp=25）
{"status":"ok","acked":true}
==> 10/11 MQTT 数据面（mosquitto）
收到: devices/default/sensor-01/telemetry {"temperature":28.74,...}
==> 11/11 清理演示资源（podsync delete → 容器回收）
{"status":"ok","acked":true}
==> DEMO PASS ✅
```

## 7. 排障

| 现象 | 原因与处理 |
|------|-----------|
| `DEMO FAIL` + 日志路径 | 按输出路径查看 `cloudcore.log` / `edgecore.log` / `mqtt.log`；修复后重跑（脚本幂等） |
| 节点未注册为 Ready | 端口冲突（换端口重试或设 `EDGEFLOW_DEMO_HUB_PORT`）；防火墙；edgecore 日志看 EdgeHub 错误 |
| 容器未创建 | Docker daemon 未运行（`docker info`）；镜像拉取慢（首次）；edgecore 日志看 Edged 调谐错误 |
| Pod 状态一直为空 | 上报周期默认 30s，等待或设 `EDGEFLOW_DEMO_*` 之外直接给 edgecore 设 `EDGEFLOW_EDGECORE_REPORT_INTERVAL=3s` |
| 设备无数据 | mock_sensor 启动即采集（2s 周期）；上报周期默认 30s 可设 `EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=3s` |
| MQTT 段被跳过 | 未检测到 mosquitto broker（macOS brew 路径 `/opt/homebrew/sbin/mosquitto` 脚本会自动识别）；`brew install mosquitto` 后重跑 |
| 遥测收不到 | broker 端口与 `EDGEFLOW_EDGECORE_MQTT_ADDR` 一致；edgecore 日志确认 `EventBus 已连接`；MQTT 模式在 edgecore 启动时装配，改地址需重启 edgecore |
| 端口被占用 | 脚本自动选随机空闲端口；固定端口场景检查 `lsof -iTCP:<port>` |

## 8. 与 KubeEdge 温度传感器示例的对应关系

| KubeEdge 示例 | EdgeFlow Demo | 说明 |
|---------------|---------------|------|
| 云端下发 Pod（Deployment） | `POST /api/v1/nodes/{nodeID}/podsync` | EdgeFlow 为云边直连模型，无需集群；Edged 保证副本与自愈 |
| Device CRD + DeviceModel | mock_sensor Mapper + `/api/v1/devices` | 数字孪生语义一致：desired（云端下发）/ reported（设备上报） |
| MQTT broker 数据面 | mosquitto + EventBus | 主题语义一致：`devices/<ns>/<device>/telemetry`、`.../command` |
| 温度传感器模拟器 | `mappers/mock_sensor`（2s 波动 + targetTemp 收敛） | 无硬件即可演示完整链路 |
| 真实 Modbus 设备 | `mappers/modbus` + `hack/modbus-sim` | 见 `docs/MODBUS-GUIDE.md`，设 `EDGEFLOW_MODBUS_ADDR` 即接入 |

## 9. 延伸阅读

- `docs/ARCHITECTURE.md`（9.1）：整体架构与模块职责
- `docs/API-SPEC.md`（9.2）：全部 REST 端点参考
- `docs/DEPLOYMENT.md`（9.3）：生产部署（Helm/keadm/mTLS）
- `docs/MAPPER-GUIDE.md`：Mapper 框架与设备接入指南
- `docs/EVENTBUS-GUIDE.md`：MQTT 数据面细节
- `docs/MODBUS-GUIDE.md`：Modbus 协议接入
