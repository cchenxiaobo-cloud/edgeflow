# EdgeFlow 部署文档（v0.1.0 定稿）

> - 对应 ROADMAP WBS 9.3「部署指南」：快速开始（5 分钟跑通）、镜像构建（WBS 8.5）、Helm Chart 部署、mTLS 启用、keadm 安装、升级回滚、卸载清理。
> - 状态：✅ **v0.1.0 定稿**（2026-08-14）。**v0.4.0 开发轮已更新**（2026-08-24）：§2.4 values 表（PVC/etcd/资源上调）、§4（/data 持久卷口径）、§5.2（升级语义）、新增 §10（云端持久化配置/备份恢复/坏库降级/replicaCount 约束）。评审记录见 `docs/REVIEWS.md`（9.3 评审归档）。
> - 组件：`cloudcore`（云端）、`edgecore`（边缘端）、`keadm`（安装管理 CLI）。
> - 配套文档：`docs/KEADM.md`（keadm 完整用法）、`docs/UPGRADE.md`（升级回滚专项，已实现 WBS 10.2）、`examples/README.md`（温度传感器 Demo 教程）。

---

## 0. 快速开始（5 分钟跑通）

单机开发/验证环境（本机同时跑 cloudcore + edgecore，无需 Kubernetes 集群）。

### 0.1 前置条件

| 依赖 | 版本要求 | 说明 |
|------|---------|------|
| Go | 1.26+ | 构建二进制（或直接用发布产物） |
| Make | 任意 | `make build` |
| Docker | 运行中 | Edged 的容器运行时（多副本 Pod/自愈依赖） |
| mosquitto | 可选 | MQTT 数据面（缺失时设备链路降级为纯本地模式，主链路不受影响） |

### 0.2 方式一：一键 Demo（推荐，完整端到端）

```bash
cd edgeflow
bash examples/demo.sh
```

脚本自动完成：构建 → 随机空闲端口启动 cloudcore/edgecore（临时目录）→ 验证节点注册 → 下发 nginx Pod → 验证容器/Pod 状态 → 验证设备数据流（mock_sensor）→ 设备指令 → MQTT 数据面（检测到 mosquitto 才跑）→ 清理。最终输出 **DEMO PASS**。幂等，可重复运行。

### 0.3 方式二：手动分步

```bash
# 1. 构建
make build

# 2. 启动云端（终端 A）
./bin/cloudcore
# 预期日志: HTTP server listening on :8080

# 3. 验证健康检查
curl http://127.0.0.1:8080/healthz
# {"status":"ok","version":{...}}

# 4. 启动边缘端（终端 B；本地 Docker 运行时 + 模拟传感器）
EDGEFLOW_EDGECORE_NODE_ID=edge-node-1 ./bin/edgecore
# 预期日志: EdgeHub connecting to ws://127.0.0.1:10000 as edge-node-1

# 5. 验证节点注册（终端 A）
curl http://127.0.0.1:8080/api/v1/nodes
# 预期: 包含 nodeID=edge-node-1、status=Ready 的 JSON

# 6. 下发 Pod（nginx）
curl -X POST http://127.0.0.1:8080/api/v1/nodes/edge-node-1/podsync \
  -d '{"operation":"add","pod":{"name":"nginx","namespace":"default","image":"nginx:1.25-alpine","replicas":1}}'
# {"status":"ok","acked":true}

# 7. 查看容器与设备数据
docker ps --filter label=edgeflow.pod        # edgeflow-default-nginx-0
curl http://127.0.0.1:8080/api/v1/devices    # sensor-01: temperature/humidity
```

> 手动分步的完整教程与预期输出见 `examples/README.md`（9.5 温度传感器端到端 Demo）。

---

## 1. 镜像构建

### 1.1 构建命令（仓库根目录执行）

```bash
# cloudcore 运行镜像
docker build -f build/docker/Dockerfile --target cloudcore -t edgeflow/cloudcore:v0.1.0 .

# edgecore 运行镜像
docker build -f build/docker/Dockerfile --target edgecore -t edgeflow/edgecore:v0.1.0 .
```

### 1.2 构建参数（可选）

版本信息默认与 `Makefile` 的 `VERSION` 一致（v0.1.0），可覆盖：

```bash
docker build -f build/docker/Dockerfile --target cloudcore \
  --build-arg VERSION=v0.2.0 \
  --build-arg GIT_COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILD_TIME=$(date +%Y-%m-%dT%H:%M:%S%z) \
  -t edgeflow/cloudcore:v0.2.0 .
```

### 1.3 验证

```bash
# 版本信息（-ldflags 注入，容器内可直接验证）
docker run --rm edgeflow/cloudcore:v0.1.0 --version
docker run --rm edgeflow/edgecore:v0.1.0 --version

# 健康检查冒烟测试（cloudcore）
docker run -d --name ef-smoke -p 18080:8080 edgeflow/cloudcore:v0.1.0
curl http://127.0.0.1:18080/healthz
docker rm -f ef-smoke

# 确认镜像
docker images | grep edgeflow
```

### 1.4 设计说明

| 项目 | 选择 | 理由 |
| --- | --- | --- |
| 构建镜像 | `golang:1.26-alpine` | 与 go.mod（go 1.26.2）匹配，alpine 体积小 |
| 编译方式 | `CGO_ENABLED=0` 静态编译 | SQLite 使用 modernc（纯 Go），无需 cgo；产物无动态依赖 |
| 运行镜像 | `gcr.io/distroless/static-debian12:nonroot` | 无 shell/包管理器，攻击面最小；内置非 root（uid 65532）；代价是容器内无法 exec 调试，排障用 `docker cp` 拉日志 |
| 端口 | cloudcore：8080（HTTP/healthz）+ 10000（CloudHub）；edgecore：无入站端口 | 与代码默认值一致，edgecore 如需 MQTT broker 可后接 sidecar（1883） |
| 数据 | edgecore 的 SQLite 写入 `/data`（已预授权 nonroot） | 生产建议挂载持久卷；**v0.4.0 起 cloudcore 亦写 `/data`（嵌入式 etcd，`/data/etcd`）**，Helm 默认 PVC 挂载 |

### 1.5 多架构镜像

```bash
# 交叉编译二进制（linux/amd64 + linux/arm64，产物在 dist/）
make cross-build

# 用 buildx 构建并推送多架构镜像（详见 docs/MULTIARCH.md）
docker buildx build --platform linux/amd64,linux/arm64 \
  -f build/docker/Dockerfile --target cloudcore \
  -t registry.example.com/edgeflow/cloudcore:v0.1.0 --push .
```

### 1.6 推送镜像

构建产物需推送后才能被 Kubernetes 拉取（当前未推送，见「缺口与风险」）：

```bash
# 推送到私有仓库（示例：registry.example.com/edgeflow）
docker tag edgeflow/cloudcore:v0.1.0 registry.example.com/edgeflow/cloudcore:v0.1.0
docker push registry.example.com/edgeflow/cloudcore:v0.1.0
# edgecore 同理；私有仓库需在 Chart 中配置 imagePullSecrets
```

---

## 2. Helm 部署（CloudCore）

### 2.1 前置条件

- Kubernetes 集群（本地验证可用 `helm install --dry-run=client`，无需真实集群）
- Helm v3+（本仓库验证环境为 Helm v4.2.3）

### 2.2 安装

```bash
cd edgeflow

# 默认配置安装（镜像 edgeflow/cloudcore:v0.1.0，ClusterIP）
helm install edgeflow build/charts/edgeflow

# 自定义 values 安装
helm install edgeflow build/charts/edgeflow -f my-values.yaml

# 覆盖关键参数
helm install edgeflow build/charts/edgeflow \
  --set cloudcore.image.repository=registry.example.com/edgeflow/cloudcore \
  --set cloudcore.image.tag=v0.1.0 \
  --set cloudcore.replicaCount=1
```

### 2.3 验证部署

```bash
kubectl get deploy,svc,pods -l app.kubernetes.io/instance=edgeflow

# 端口转发后验证健康检查
kubectl port-forward svc/edgeflow-cloudcore 8080:8080
curl http://127.0.0.1:8080/healthz
# 预期返回：{"status":"ok","version":{...}}
```

### 2.4 关键 values 说明

| values 路径 | 默认值 | 说明 |
| --- | --- | --- |
| `cloudcore.image.repository` / `.tag` / `.pullPolicy` | `edgeflow/cloudcore` / `v0.1.0` / `IfNotPresent` | 镜像地址与拉取策略；私有仓库改地址并配 `imagePullSecrets` |
| `cloudcore.replicaCount` | `1` | 副本数；**硬约束（v0.4.0 起）**：嵌入式 etcd 为单成员，多副本各自 embed 会脑裂，必须保持 1 |
| `cloudcore.port.http` / `.hub` | `8080` / `10000` | 容器监听端口（与 Dockerfile EXPOSE 对齐） |
| `cloudcore.env` | `EDGEFLOW_CLOUDCORE_PORT=8080`、`EDGEFLOW_CLOUDCORE_HUB_PORT=10000`、`EDGEFLOW_CLOUDCORE_ETCD_*`（v0.4.0，见下方 etcd 段） | 环境变量透传（key/value 形式） |
| `cloudcore.extraEnv` | `[]` | 扩展 env 列表（valueFrom/secretKeyRef 等复杂引用） |
| `cloudcore.etcd.enabled` / `.dataDir` | `true` / `/data/etcd` | v0.4.0 嵌入式 etcd 总开关与数据目录（容器内路径，PVC 挂载 `/data`） |
| `cloudcore.etcd.persistence.enabled` / `.storageClass` / `.size` | `true` / 空（默认 StorageClass） / `1Gi` | 数据卷 PVC 开关/存储类/容量；**Pod 重建不丢数据的前提**（emptyDir 会丢） |
| `cloudcore.etcd.env.*` | `EDGEFLOW_CLOUDCORE_ETCD_*` 全量默认（见 §10.2 配置表） | etcd 配置透传：quota 256MiB / compaction periodic 1h / 回环端口 12379/12380 / STRICT 默认 off |
| `cloudcore.livenessProbe` / `.readinessProbe` | 10s/5s 起检，间隔 10s/5s | 探针路径固定 `/healthz`（端口按名称 `http` 引用） |
| `cloudcore.resources` | requests 100m/256Mi；limits 500m/1Gi | v0.4.0 上调（嵌入式 etcd，实测 RSS 31-34MB，128Mi 接近上限）；生产仍建议压测 |
| `cloudcore.nodeSelector` / `.tolerations` / `.affinity` | 空（注释示例） | 节点调度、污点容忍、亲和性 |
| `podSecurityContext` | runAsNonRoot + uid 65532 | 与 distroless nonroot 镜像匹配 |
| `service.type` / `.httpPort` / `.hubPort` / `.hubEnabled` | ClusterIP / 8080 / 10000 / false | 集群内访问；边缘节点在集群外时需 NodePort/LoadBalancer + `hubEnabled=true` |
| `global.environment` | production | 部署环境标签 |

### 2.5 边缘节点接入（集群外）

1. 暴露 CloudHub 端口：

   ```bash
   helm upgrade edgeflow build/charts/edgeflow \
     --set service.hubEnabled=true --set service.type=NodePort
   kubectl get svc edgeflow-cloudcore   # 记录 hub 端口对应的 NodePort
   ```

2. 边缘节点运行 `edgecore`（二进制或容器均可），通过环境变量连接：

   ```bash
   export EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://<节点IP>:<NodePort>/v1/edge
   export EDGEFLOW_EDGECORE_NODE_ID=edge-node-1
   ./bin/edgecore
   ```

   或容器方式（挂载持久卷给 `/data`）：

   ```bash
   docker run -d --name edgecore \
     -e EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://<节点IP>:<NodePort>/v1/edge \
     -e EDGEFLOW_EDGECORE_NODE_ID=edge-node-1 \
     -v /var/lib/edgeflow:/data \
     edgeflow/edgecore:v0.1.0
   ```

---

## 3. keadm 安装（离线产物生成）

`keadm` 是 EdgeFlow 的安装管理 CLI（对标 KubeEdge keadm）。v0.1.0 为基础版：**只做离线产物生成**，生成物由用户拿到真实集群/边缘节点上执行（详见 `docs/KEADM.md`）。

```bash
# 构建
go build -o bin/keadm ./cmd/keadm

# 云端：生成 cloudcore.yaml（Deployment + Service）+ NOTES.txt
./bin/keadm init --output-dir=./keadm-out
# 生产推荐启用 mTLS 并注入 SAN：
./bin/keadm init --tls --tls-san=IP:192.168.1.10 --output-dir=./keadm-out

# 边缘：生成 edgecore.env + edgecore.service + install.sh + README.md
./bin/keadm join --cloudcore-ip=192.168.1.10 --token=<token> --node-id=edge-01 --output-dir=./keadm-out

# 在真实集群上执行产物
kubectl apply -f keadm-out/cloudcore.yaml
# 边缘节点执行安装脚本
bash keadm-out/install.sh

# 清理生成产物（确认后删除，幂等）
./bin/keadm reset --output-dir=./keadm-out
```

> 升级/回滚命令（`keadm upgrade` / `keadm rollback`）已实现（WBS 10.2），用法见 `docs/UPGRADE.md`。

---

## 4. 与 mTLS / 配置体系的衔接

- **配置优先级**：命令行 `--port` > 环境变量 `EDGEFLOW_CLOUDCORE_PORT` > 配置文件 `config/cloudcore.json` > 默认值。
  容器内默认不内置配置文件，通过 Chart 的 `cloudcore.env` 透传环境变量即可完成端口等配置；
  需要更复杂配置时，可将配置文件以 ConfigMap 挂载并用 `--config` 覆盖（`extraEnv` 或 args 传入）。
- **热重载**（WBS 2.7，cloudcore/edgecore 均支持）：修改配置文件后向进程发送 `SIGHUP` 立即生效，
  或等待最长 60s 自动生效（mtime 变化检测）。热生效范围：cloudcore `port`（HTTP/healthz 监听热切换，
  绑定失败自动回滚旧监听）、edgecore 上报周期（`podReportInterval`/`deviceReportInterval`）；
  `hubPort`/`compress`（cloudcore）、`cloudAddr`/`nodeID`/`reconcileInterval`（edgecore）变更需重启生效
  （重载时记录警告并保持旧值）。重载失败（JSON 错误/校验失败/端口被占）自动保持旧配置继续运行，不影响在线业务。
- **mTLS**：CloudHub（10000 端口）启用 mTLS 后：
  - 证书可通过 `cloudcore.env` 启用（`EDGEFLOW_CLOUDCORE_TLS=on` + `EDGEFLOW_CLOUDCORE_CERT_DIR=/data/certs`，
    /data 挂载持久卷（v0.4.0 起为 PVC，默认 emptyDir 已替换），首次启动自动生成证书；生产环境建议把证书预置为
    Secret 并以 extraEnv 注入 `EDGEFLOW_CLOUDCORE_CERT_DIR` 指向只读挂载路径；
  - 证书文件以 Secret 挂载（`volumeMounts` + `volumes`，Chart 预留了 `extraEnv` 扩展位，
    卷挂载可按需在模板中追加，或后续版本内置 `cloudcore.tls` 字段）；
  - edgecore 侧 `EDGEFLOW_EDGECORE_CLOUD_ADDR` 相应改为 `wss://...` 并配置客户端证书
    （`EDGEFLOW_EDGECORE_TLS=on` + `EDGEFLOW_EDGECORE_CERT_DIR`，与云端共享同一 CA 目录）。
- **edgecore 数据**：SQLite 元数据库默认写 `data/edgeflow.db`（容器内为 `/data/edgeflow.db`），
  通过 `EDGEFLOW_EDGECORE_DB_PATH` 可重定向，生产务必挂载持久卷。

### 4.1 mTLS 启用检查清单

```bash
# 云端（自动生成 CA + 服务端证书）
EDGEFLOW_CLOUDCORE_TLS=on EDGEFLOW_CLOUDCORE_CERT_DIR=/data/certs ./bin/cloudcore

# 边缘（自动生成/加载 CA + 客户端证书，ws:// 自动升级 wss://）
EDGEFLOW_EDGECORE_TLS=on EDGEFLOW_EDGECORE_CERT_DIR=/data/certs ./bin/edgecore

# 跨主机部署时云端需注入 SAN（边缘访问地址）
EDGEFLOW_CLOUDCORE_TLS_SAN='IP:10.0.0.5,DNS:cloudcore.svc' ./bin/cloudcore

# 证书查看/重建脚本
bash hack/gen-certs.sh --help
```

---

## 5. 升级 / 回滚

> 本仓库升级回滚专项（keadm 侧 `upgrade`/`rollback` 命令 + 二进制 OTA 流程）见 **`docs/UPGRADE.md`**（已实现 WBS 10.2）。

### 5.1 Helm 管理面（现有可用路径）

```bash
# 升级（install 与 upgrade 通用，幂等）
helm upgrade --install edgeflow build/charts/edgeflow -f values-prod.yaml

# 查看发布历史
helm history edgeflow

# 回滚到指定版本（REVISION 以 helm history 输出为准）
helm rollback edgeflow 1
```

升级注意：
- Chart 版本变化（`Chart.yaml version` 递增）时使用新 Chart 目录重新 `helm upgrade`；
- 镜像变更走 `--set cloudcore.image.tag=...` 或更新 values 文件；
- 回滚仅恢复 Helm 管理的资源，不恢复数据（数据库在 PVC 中）。

### 5.2 二进制/边缘节点升级注意

- edgecore 升级建议先摘流（停业务 Pod）再替换二进制，最后 `docker ps` 复核容器由新版本 Edged 接管；
- 云端 cloudcore **v0.4.0 起分级持久化**：注册台账与设备 Desired 跨重启保留（嵌入式 etcd 写穿）；Pod 状态与上报属性重启后短暂清空，边缘重连 ≤1 上报周期自愈；v0.3.0 及以前为纯内存态（重启后重新注册恢复）；
- SQLite 元数据库（`data/edgeflow.db`）跨小版本兼容，升级前建议备份（`cp data/edgeflow.db data/edgeflow.db.bak`）。

---

## 6. 卸载 / 清理

### 6.1 Helm 部署卸载

```bash
# 卸载（注意：不删除 PVC 等持久资源）
helm uninstall edgeflow

# 需要连 PVC 一起删时（数据不可恢复，谨慎）
kubectl delete pvc -l app.kubernetes.io/instance=edgeflow
```

### 6.2 单机开发环境清理

```bash
# 停止 cloudcore / edgecore（Ctrl+C 或 kill -TERM，均支持优雅退出）
# 清理 Edged 管理的容器（按标签精确删除，不影响其他容器）
docker rm -f $(docker ps -aq --filter label=edgeflow.pod) 2>/dev/null

# 清理本地 SQLite 元数据（默认 data/edgeflow.db；生产环境请按备份策略处理）
rm -f data/edgeflow.db

# 一键 Demo 的资源由脚本自动清理；残留时：
#  - 进程: pkill -f 'bin/edgecore'; pkill -f 'bin/cloudcore'
#  - 临时目录: rm -rf ${TMPDIR:-/tmp}/edgeflow-demo.*
```

### 6.3 keadm 产物清理

```bash
./bin/keadm reset --output-dir=./keadm-out   # 删除 keadm 生成的全部产物（确认后执行）
```

---

## 7. 故障排查

| 现象 | 排查 |
| --- | --- |
| Pod 一直未 Ready | `kubectl describe pod` 看探针结果；确认 `/healthz` 返回 200 |
| ImagePullBackOff | 镜像未推送/私有仓库未配置 `imagePullSecrets`；本机验证用 `pullPolicy: IfNotPresent` + 已构建镜像 |
| 容器内无法 exec | distroless 无 shell，用 `kubectl logs` / `docker cp` 拉日志 |
| 边缘节点连不上 | 确认 Service 的 hub 端口已暴露（`hubEnabled=true`）、防火墙放行、`EDGEFLOW_EDGECORE_CLOUD_ADDR` 协议与端口正确 |
| 权限错误 | distroless nonroot（uid 65532）需要挂载卷可写：emptyDir 默认可写；PV 需配置 fsGroup 或属主 65532 |
| 设备无数据上报 | 确认 Mapper 采集循环运行（edgecore 日志 `MockSensor ... 启动`）；设备上报周期默认 30s，可设 `EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=3s` 加速观察 |
| Pod 容器未创建 | 确认 Docker daemon 运行（`docker info`）；edgecore 日志看 `Edged` 调谐错误；镜像本地未缓存时首次拉取需要时间 |
| MQTT 数据面不工作 | `brew install mosquitto`（macOS 的 broker 在 `/opt/homebrew/sbin/mosquitto`）；确认 `EDGEFLOW_EDGECORE_MQTT_ADDR` 指向 broker |

---

## 8. 验证清单（v0.1.0 定稿时已执行）

- [x] `make build` 成功（cloudcore/edgecore）
- [x] `docker build --target cloudcore` / `--target edgecore` 成功（Docker 29.4.3）
- [x] `docker run --rm edgeflow/cloudcore:v0.1.0 --version` 输出 `version=v0.1.0 ...`
- [x] 容器冒烟：`/healthz` 返回 200 + `{"status":"ok",...}`；CloudHub 监听 10000
- [x] `helm lint build/charts/edgeflow` 通过（0 failed）
- [x] `helm template` 渲染检查：image / 探针 / env / 资源齐全
- [x] `helm install --dry-run=client` 通过（无需真实集群）
- [x] keadm `init` / `join` / `reset` / `version` 生成产物验证（见 docs/KEADM.md）
- [x] **一键端到端 Demo 两次完整通过（DEMO PASS）**：`bash examples/demo.sh`（含 MQTT 数据面；清理后无残留进程/容器/临时目录）

## 9. 缺口与风险（v0.1.0 定稿时确认）

| 缺口/风险 | 说明 | 计划 |
|----------|------|------|
| 镜像未推送到公共/私有仓库 | 单机构建可用，集群拉取需先推送 | 发布流程（M5 发布物） |
| mTLS 证书自动生成默认 SAN 仅本机 | 跨主机部署必须注入 `EDGEFLOW_CLOUDCORE_TLS_SAN` | 文档已覆盖，后续支持配置文件方式 |
| 云边升级回滚专项依赖 UPGRADE.md | keadm upgrade/rollback 已实现（WBS 10.2） | 本文档 §5 保持引用 |
| 边缘节点资源上报（CPU/内存）未采集 | `/api/v1/nodes` 的 memory 恒 0 | 后续版本 |

---

## 10. 云端持久化（v0.4.0 起：嵌入式 etcd）

> v0.4.0 核心变更：cloudcore 内嵌单成员 etcd（`go.etcd.io/etcd/v3` embed，v3.5.33）作为云端状态**持久化事实源**（写穿 write-through）。设计见 ARCHITECTURE.md 决策 R13；键空间/持久化范围详见 RELEASE-NOTES-v040.md。

### 10.1 概述

- **写穿**：需要持久化的写（节点注册 Register、设备 Desired 的 SetDesired、GC 删除）**先写 etcd 成功、再更新内存缓存**——"写成功 = 已持久化"，云 core 崩溃/重启不丢。
- **读路径**：全部走内存缓存（HTTP 热路径毫秒级响应，永不读 etcd）；启动时前缀 Range 全量加载（Load → Seed），Status=Unknown、LastHeartbeatAt=0，等边缘心跳翻新。
- **不落盘**：心跳、Status/LastHeartbeatAt、Pod 状态整表、设备 Properties/LastReportedAt、Offline 标记——重启后短暂清空（≤1 上报周期，边缘重连自愈），无写放大、无陈旧脏状态。
- **默认启用**：`EDGEFLOW_CLOUDCORE_ETCD_ENABLED=true`（默认）；设为 `false` 完全退回 v0.3.x 纯内存（不建目录、不占端口、不写盘）。

### 10.2 持久化配置表（环境变量）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_ETCD_ENABLED` | `true` | 总开关；`false` = 纯内存（v0.3.x 行为） |
| `EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR` | `data/etcd` | embed 数据目录（相对工作目录，自动 MkdirAll；容器内为 `/data/etcd`，挂载自 PVC） |
| `EDGEFLOW_CLOUDCORE_ETCD_CLIENT_URL` | `http://127.0.0.1:12379` | 客户端监听；**只绑 127.0.0.1**（非回环 URL 拒绝启动） |
| `EDGEFLOW_CLOUDCORE_ETCD_PEER_URL` | `http://127.0.0.1:12380` | peer 监听（单成员，仅 embed 内部） |
| `EDGEFLOW_CLOUDCORE_ETCD_QUOTA_BACKEND_BYTES` | `268435456`（256MiB） | 后端配额（对齐风险审查 quota ≤256MB；拒绝 etcd 默认 2GB） |
| `EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_MODE` | `periodic` | 自动压缩模式（`periodic` / `revision`） |
| `EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_RETENTION` | `1h` | 压缩保留期（periodic 模式为时长；`1h` 或秒数） |
| `EDGEFLOW_CLOUDCORE_ETCD_STRICT` | 空（off） | `1` 时 embed 启动失败 = **拒绝启动**（fail-fast）；默认关 = 降级内存 + 告警 |
| `EDGEFLOW_CLOUDCORE_NODE_RETENTION` | `24h` | 节点保留期（喂注册表 OfflineTTL + etcd GC 阈值，内存/etcd 同一口径） |

配置解析 fail-fast（非法值报错退出，风格对齐 `nodecontroller.DurationsFromEnv`）。

### 10.3 数据目录布局

```
data/etcd/
├── member/
│   ├── snap/          # raft 快照
│   └── wal/           # raft WAL（崩溃安全的核心：kill -9 不丢已提交写）
└── <lock>             # embed 数据目录锁
```

备份内容 = 数据目录 + 快照文件；`data/etcd` 之外无其他云端状态。

### 10.4 备份 / 恢复 runbook

**⚠️ 文件拷贝 ≠ 有效备份**：etcd 的 WAL 与 snapshot 必须保持一致，运行中只拷贝个别文件 = 得到一份损坏库。只有两种合法备份方式：

**① 在线快照（推荐，cloudcore 运行中）**：

```bash
etcdutl snapshot save --endpoints=http://127.0.0.1:12379 data/backups/etcd-snapshot-$(date +%s).db
# embed 成员是标准 etcd 成员，etcdutl 直接可用；建议封装 cron 定时
```

**② 离线冷备（最简，需停 cloudcore）**：

```bash
# 先停 cloudcore（优雅关停，SIGTERM）
cp -a data/etcd data/backups/etcd-$(date +%s)   # 必须整体拷贝目录且进程已停止
```

**恢复（快照）**：

```bash
# 1. 停 cloudcore
# 2. 恢复到全新目录（绝不覆盖现有数据目录）
etcdutl snapshot restore <backup.db> --data-dir=data/etcd-restored
# 3. 用恢复目录启动（或移入原 data/etcd 位置）
EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR=data/etcd-restored ./bin/cloudcore
# 4. 校验恢复
curl http://127.0.0.1:8080/api/v1/nodes
```

**无备份的故障兜底**：清空数据目录重启 → 边缘节点重连重新注册收敛，设备 Desired 丢失后由指令重发重建（小规模可接受，规模生产应走快照备份）。

### 10.5 坏库降级行为

| 场景 | 默认（STRICT off） | `EDGEFLOW_CLOUDCORE_ETCD_STRICT=1` |
|------|--------------------|-----------------------------------|
| 目录不可写 / 端口被占 / 无法恢复 | **降级纯内存模式 + 启动期大告警**（"数据未持久化，重启将丢失"），进程继续服务 | **拒绝启动**（fail-fast，对持久化有硬要求的部署） |
| **坏 WAL**（`member/wal/` 内容损坏） | 实测 etcd 在 raft 恢复阶段 **panic**（非 error 返回）；装配层 `defer recover()` 兜底 → 按左列降级内存 + 告警，进程不裸崩 | recover 后按 fail-fast 退出 |
| 加载时单键 JSON 反序列化失败 | 跳过该键 + 告警（坏键不阻断全库），统计告警条数 | 同左（加载坏键不阻断） |
| `member/` 目录整体丢失 | 重建空库正常启动（旧数据不在、新写可用） | 同左 |

降级后恢复持久化：修复数据目录/端口后重启（**不热切换**，v0.4.0 不实现运行中重挂）。坏 WAL 修复路径：从备份 restore 或清空数据目录重收敛。

### 10.6 Helm 部署注意（replicaCount 与 PVC）

- **`replicaCount` 必须 = 1（硬约束）**：嵌入式 etcd 为单成员；多副本各自 embed 单成员 + 共享 PVC = **脑裂**（各自独立的数据分叉）。Chart 注释已显式禁止，勿突破。**v0.5.0 起外部模式同样必须 = 1**（单写者铁律，见 §10.7），模板已加 `{{ fail }}` 渲染守卫双保险。
- **默认创建 PVC**：`cloudcore.etcd.persistence.enabled=true`（默认），storageClass 留空 = 集群默认 StorageClass，容量默认 1Gi；`false` 时回退 emptyDir（**Pod 重建即丢数据，仅供无持久化需求的临时环境**）。
- **资源上调**：requests 256Mi / limits 1Gi（v0.4.0 Chart 默认；实测 RSS 31-34MB，原 128Mi request 接近上限）。
- etcd 客户端监听在容器内 127.0.0.1:12379/12380，**无需也不应**暴露为 Service 端口（仅进程内访问）。
- 外部 etcd 模式的 Helm 配置与部署形态见 **§10.7**。

---

### 10.7 外部 etcd 模式（v0.5.0 起，方案④）

> 核心能力：`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 非空 = cloudcore **直连共享 etcd 集群**（跳过内嵌 embed），注册台账与设备 Desired 的持久化事实源外移到独立集群。业务层（registry/devicestatus/HTTP API）零改动（v0.4.0 冻结的 KVStore 接口即替换面）。设计见 ARCHITECTURE.md 决策 R14；限制登记见 KNOWN-ISSUES.md §5。

#### 10.7.1 模式对比与故障语义

| 维度 | embed 模式（默认，v0.4.0） | 外部模式（v0.5.0 起） |
|------|---------------------------|----------------------|
| 触发 | `ENDPOINTS` 未设置/为空（v0.4.0 行为逐位不变） | `ENDPOINTS` 非空（逗号分隔 http(s) URL） |
| 数据位置 | 本机数据目录（`EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR`） | 共享集群（`/edgeflow/` 键空间） |
| 启动失败语义 | 默认**降级纯内存 + 告警**；`STRICT=1` 才拒绝启动 | **恒 fail-fast 拒绝启动**（显式依赖不静默降级；`STRICT` 对外部模式无意义） |
| embed 字段（DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT） | 生效 | **全部忽略**；显式设置时启动期 `Warn`"仅 embed 生效" |
| 端口/目录 | 监听 127.0.0.1:12379/12380，创建数据目录 | **不监听任何 etcd 端口、不创建数据目录**（验收项） |
| 运行中断连 | 本地故障（罕见） | clientv3 **自动重连**（指数退避），应用层零重试；断连期写路径失败返回 error、内存不动，读路径纯内存不受影响，恢复后自愈 |
| /healthz | 进程存活（不反映 etcd） | **同样不反映 etcd 连接状态**（有意为之，见 §10.7.6） |
| 副本数 | **必须 1**（多副本 embed + 共享 PVC = 脑裂） | **同样必须 1**（单写者铁律，见下方 ⛔ 段） |

**⛔ 单写者形态铁律（v0.5.0）**：两种模式都只支持**一个 cloudcore 写入方**。外部模式虽然共享同一键空间（写穿保证 etcd 侧一致、无数据分叉），但**心跳/Offline 判活是各副本内存瞬态**（不落盘）：副本 A 看不到连在副本 B 上的活节点心跳 → 180s+保留期后 A 会把它**从共享键空间判死删除并级联删设备 Desired**——活节点数据丢失且无任何信号。因此"外部集群支持多 cloudcore 副本"的表述是**错误的**，v0.5.0 受支持形态 = 单写者（replicaCount=1 或 active/standby 同一时刻仅一个真在服务）；真多活（etcd lease 心跳）划 v0.6+。Chart 模板已加 `{{ fail }}` 渲染守卫，embed 与外部模式 `replicaCount>1` 都会渲染失败。

#### 10.7.2 配置表（新增环境变量，前缀 `EDGEFLOW_CLOUDCORE_`）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` | 空（embed） | 逗号分隔的 etcd 客户端端点（http/https URL，如 `http://10.0.0.11:2379,http://10.0.0.12:2379`）；**非空即外部模式**。逐条目校验 fail-fast：空条目/非合法 URL/缺端口/带路径或 query/带 userinfo/混合 scheme 均拒绝启动（host 为回环**允许**——外部模式不做回环限制） |
| `EDGEFLOW_CLOUDCORE_ETCD_TLS_CA` | 空 | **非空 = 启用 TLS**（服务端证书校验）：指向 PEM CA 文件路径（容器内挂载后路径）；文件不存在/不可读 → 拒绝启动；启用时**全部端点必须 https**（混配 http 拒绝启动） |
| `EDGEFLOW_CLOUDCORE_ETCD_TLS_CERT` | 空 | mTLS 客户端证书 PEM 路径；**与 KEY 同设即 mTLS**，只设其一 → 拒绝启动；证书 CN 建议 `edgeflow`（供 etcd 侧 `--client-cert-allowed-cn` 使用，v0.5.0 不做 CN→角色映射，见 KNOWN-ISSUES §5 ⑤） |
| `EDGEFLOW_CLOUDCORE_ETCD_TLS_KEY` | 空 | mTLS 客户端私钥 PEM 路径（与 CERT 同设/同缺） |
| `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE` | 空（off） | **逃生门**：`1` 时允许「非回环端点 + 无 TLS」启动（启用瞬间打大告警）；默认状态下存在非回环端点且未启用 TLS → **拒绝启动**（见 §10.7.3） |

> 外部模式下 v0.4.0 embed 变量（DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT）**不解析、不生效**（显式设置仅启动期 Warn）。`ETCD_ENABLED=false` 总开关**优先于一切**（含 ENDPOINTS/TLS 配错也不报错——死路径不阻断逃生）；外部模式连接失败/无 quorum = 拒绝启动，"纯内存逃生"只有 `ETCD_ENABLED=false` 一条路。

#### 10.7.3 明文护栏：非回环 + 无 TLS = 拒绝启动

外部 etcd 是**网络可达的共享数据源**——默认明文 http + 无鉴权时，任何能路由到 2379 的客户端都能读写全部键空间（节点台账 + 设备 Desired，Desired 是边缘指令事实源，篡改即指令投毒）。因此 v0.5.0 提供**代码级护栏**：

- 存在**非回环端点**（非 127.0.0.1/localhost/::1）**且未启用 TLS** → 装配期**拒绝启动**，报错指引二选一：配置 `EDGEFLOW_CLOUDCORE_ETCD_TLS_CA`（推荐），或显式逃生门 `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE=1`（仅限可信内网/开发，开启时启动期大告警）。
- 逃生门 ≠ 安全：明文仍然可嗅探、可写入；生产环境必须 TLS（服务端校验）或 mTLS（双向，客户端证书兼作 cloudcore 身份，配合 etcd 侧 `--client-cert-auth`）。
- **外部 etcd 键空间即安全边界**：与 cloudcore API Token（`EDGEFLOW_CLOUDCORE_AUTH`）是两套体系、互不替代。v0.5.0 不透传 etcd 原生鉴权（username/password/CN 映射），见 KNOWN-ISSUES §5 ⑤。

#### 10.7.4 外部 etcd 集群拓扑要点与运维基线

- **3 节点、奇数**（2/3 法定人数；4 节点不增加可用性只增加成本），成员分布跨故障域（不同节点/机架/AZ），**不跨地域**（raft 副本延迟放大写延迟，网络分区 = 可用性风险）。
- **无需负载均衡器**：cloudcore 的 ENDPOINTS 列表即客户端 failover（clientv3 round-robin + 失败转移），leader 重定向透明；etcd 无固定 leader（raft 选举），cloudcore 无需感知。
- 部署形态：独立 etcd 集群（二进制/systemd 或 etcd-operator 类编排），**与 EdgeFlow Chart 解耦**（cloudcore 只消费端点；K8s 中建议 etcd 先就绪再起 cloudcore，或加 initContainer 依赖检查——启动检查最坏 ≈17s（预算 ≤20s）内失败即拒绝启动）。
- 运维基线（集群侧配置，cloudcore 不再也不应重复设置）：
  ```bash
  # quota/compaction 对齐 embed 默认口径（三类数据量级为 MB）
  --quota-backend-bytes=268435456            # 256MiB
  --auto-compaction-mode=periodic --auto-compaction-retention=1h
  # TLS（peer + client）与最小权限鉴权（二选一或叠加；mTLS 推荐生产）
  --client-cert-auth --trusted-ca-file=/etc/edgeflow/etcd-ca.pem [--client-cert-allowed-cn=edgeflow]
  ```
- **etcdctl 最小权限**（auth enable 前置条件 = root 用户已存在；`/edgeflow/` 前缀权限覆盖 `_meta/*` 与全部业务键）：
  ```bash
  etcdctl user add root:<强密码>
  etcdctl auth enable
  etcdctl role add edgeflow-rw
  etcdctl role grant-permission edgeflow-rw readwrite /edgeflow/
  etcdctl user add edgeflow:<强密码>
  etcdctl user grant-role edgeflow edgeflow-rw
  etcdctl role add edgeflow-ro
  etcdctl role grant-permission edgeflow-ro read /edgeflow/   # 只读角色供 O&M 巡检
  # 读写分离：cloudcore 用 edgeflow-rw（网络层由 mTLS 客户端证书标识身份）；巡检工具用 edgeflow-ro
  # ⚠ 集群开启鉴权后 cloudcore 未获授权 → 启动检查 PermissionDenied → fail-fast（v0.5.0 不支持鉴权参数透传）
  ```
- 备份：每成员 WAL+snapshot + 全集群级 `etcdutl snapshot save --endpoints=<客户端端点>`（复用 §10.4 工具链）；成员级故障演练：停 1 成员 cloudcore 无感知；停 2 成员写路径失败但**不崩**，恢复后自动续写。

#### 10.7.5 迁移 runbook（embed → 外部集群）

> 工具统一 `etcdutl`（etcd 官方快照工具，与二进制同版本 3.5.x）。**⛔ 铁律**：迁移窗口内**只允许一个写入方**（先停 cloudcore 再动数据）；restore 一律到**全新目录**，绝不覆盖。

```bash
# ① 在线快照（cloudcore 运行中，embed 客户端端口按实际 CLIENT_URL）
etcdutl snapshot save --endpoints=http://127.0.0.1:12379 /opt/backups/edgeflow-$(date +%s).db
# ② 校验快照：revision/总键数符合预期
etcdutl snapshot status /opt/backups/edgeflow-<TS>.db

# ③ 停 cloudcore（双写禁止；优雅关停 SIGTERM）
#    systemctl stop edgeflow-cloudcore   # 或 kubectl scale deploy edgeflow-cloudcore --replicas=0

# ④ 恢复到外部集群——单节点外部集群：
etcdutl snapshot restore /opt/backups/edgeflow-<TS>.db \
  --data-dir=/var/lib/etcd/edgeflow-ext --name=ext-1 \
  --initial-cluster=ext-1=http://10.0.0.11:2380 --initial-cluster-token=edgeflow-v050 \
  --initial-advertise-peer-urls=http://10.0.0.11:2380
#    三节点外部集群：每节点各执行一次 restore（同源备份、各自 name/peer URL），再同时启动：
#    etcd-1: --name=ext-1 --data-dir=/var/lib/etcd/ext1 \
#            --initial-cluster=ext-1=http://10.0.0.11:2380,ext-2=http://10.0.0.12:2380,ext-3=http://10.0.0.13:2380 \
#            --initial-advertise-peer-urls=http://10.0.0.11:2380 --initial-cluster-token=edgeflow-v050
#    etcd-2/3 同理（--name=ext-2/3、各自 advertise-peer-urls）；三份 restore 必须同源同 token、
#    三个成员同时启动才能达成初始 quorum。然后按 §10.7.4 运维基线参数启动。

# ⑤ 切 cloudcore 到外部模式
export EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS="http://10.0.0.11:2379,http://10.0.0.12:2379,http://10.0.0.13:2379"
# 可选 TLS：export EDGEFLOW_CLOUDCORE_ETCD_TLS_CA=/etc/edgeflow/etcd-ca.pem
# ⑥ 启动 cloudcore（外部模式 fail-fast：连不上/无 quorum 直接拒绝启动）
# ⑦ 验证
curl http://127.0.0.1:8080/api/v1/nodes            # 节点清单完整（Status=Unknown，等心跳翻新）
etcdctl get /edgeflow/ --prefix --write-out=json | head -20   # 键空间已在外部集群
etcdctl get /edgeflow/_meta/schemaVersion          # = "1"（快照自带则保持不变）
# ⑧ 回滚保险：旧 embed 数据目录保留 ≥1 周再清理（回滚 = 恢复 ③ 前的启动方式即可）
```

要点：

- restore 生成**全新集群身份**（新 cluster ID/member ID），与原 embed（`name=edgeflow`、`initial-cluster-token=edgeflow-v0.4.0`）无冲突；restore 只搬 KV 快照，不含成员/集群元数据。单成员 embed 快照恢复成 3 节点集群是标准操作（每成员各自 restore 同一备份），反向（3→1）同样成立。
- 快照含 `/edgeflow/_meta/schemaVersion` → 恢复后键保持；空集群首次启动由 v0.5.0 自动写入。
- 迁移后 embed 的 QUOTA(256MiB)/COMPACTION(1h) 约束**由集群侧配置承接**（§10.7.4），cloudcore 侧不再重复设置。
- 外部 → 外部（集群替换/DR/扩容拓扑）：同工具同流程，仅①的 endpoints 指向旧外部集群端点；集群内成员日常增减（`etcdctl member add/remove`）属外部集群运维，不进本 runbook。

**零迁移自愈路径（可用，限定场景）**：停 cloudcore → 启动**空**外部集群 → 配好 ENDPOINTS 启动 cloudcore → 边缘节点重连时**自动重注册**（台账重建）→ Pod 状态/设备 Properties ≤1 上报周期翻新。

| 适用 | 不适用 |
|------|--------|
| 开发/演示/小规模；**设备 Desired 可丢**（无活跃 device-command 历史） | 生产有已下发指令（Desired 是云端唯一事实源，丢失后需人工/脚本重发）；对台账连续性有审计要求的环境 |

判定口诀：**"Desired 可接受重建" → 零迁移；否则走快照恢复。**

#### 10.7.6 排障

| 现象 | 排查 |
|------|------|
| 启动失败，报错含 `外部 etcd 不可达（endpoints=...）` | 懒连接探活失败：`clientv3.New` 不发起连接，v0.5.0 以**线性一致读**元键落实 fail-fast（至多 3 次尝试、单次 5s、间隔 1s，最坏 ≈17s、预算 ≤20s）。逐一检查：端点连通/防火墙（`etcdctl endpoint health --endpoints=<...>`）、DNS 可解析、集群有 quorum（1/3 存活时 Get 超时 → 拒绝启动是**预期行为**，验证的是"集群可服务"而非仅端口可达）、TLS 证书/CA 路径可读 |
| 启动失败，报错含 `鉴权被拒（PermissionDenied）` 引导文案 | 外部集群开启了鉴权而 cloudcore 无凭证：**v0.5.0 不支持鉴权参数透传**（KNOWN-ISSUES §5 ⑤），按 §10.7.4 在 etcd 侧为 `/edgeflow/` 授权（edgeflow-rw 角色）或改用 mTLS 客户端证书；救急：`ETCD_ENABLED=false` 纯内存启动（丢持久化） |
| 启动失败，报错含 "非回环端点且未启用 TLS" | 明文护栏触发（§10.7.3）：配置 `EDGEFLOW_CLOUDCORE_ETCD_TLS_CA`（推荐）或显式 `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE=1`（仅可信内网） |
| 启动失败，报错含端点校验原因（缺端口/带路径/userinfo/混合 scheme） | 端点格式错误（§10.7.2 逐条目 fail-fast），修正 ENDPOINTS |
| 运行中写失败（节点注册失败、设备指令落点报错），读 API 正常 | 外部集群故障窗口：读路径纯内存不受影响（最后一致状态）；clientv3 自动重连，恢复后下一笔写自动成功，**无需重启 cloudcore**；/healthz 恒 200（不反映 etcd，避免 K8s 批量重启放大故障——v0.5.0 有意为之）；监控建议走外部集群自身的 `etcdctl endpoint health` 与备份告警 |
| 多副本误配 | 模板渲染期 `{{ fail }}` 已拦截（embed 与外部模式 replicaCount>1 均失败）；手工裸跑二进制时请遵守单写者铁律 |

#### 10.7.7 Helm 部署（外部模式）

```yaml
cloudcore:
  replicaCount: 1          # 单写者铁律：外部模式同样必须 1（模板 fail 守卫兜底）
  etcd:
    enabled: true
    external:
      enabled: true
      endpoints:
        - http://10.0.0.11:2379
        - http://10.0.0.12:2379
        - http://10.0.0.13:2379
      tls:
        ca: /etc/edgeflow/etcd-tls/ca.crt      # 非空才注入 EDGEFLOW_CLOUDCORE_ETCD_TLS_CA
        cert: /etc/edgeflow/etcd-tls/tls.crt   # mTLS（与 key 同设）
        key: /etc/edgeflow/etcd-tls/tls.key
      allowInsecure: false  # 逃生门：true → 注入 EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE=1
    persistence:            # 外部模式不落本地盘：PVC 自动跳过、/data 回退 emptyDir
      enabled: true         # （保持 true 也不创建 PVC）
```

- 模板行为：`external.enabled=true` → 注入 `EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS`（endpoints 逗号连接）+ TLS/逃生门 env；**不注入** `ETCD_DATA_DIR` 等 embed env；**不创建 PVC**。
- 渲染守卫（`{{ fail }}`）：embed 模式 `replicaCount>1` → 失败（脑裂）；外部模式 `replicaCount>1` → 失败（单写者铁律）；`external.enabled=true` 且 `endpoints` 为空 → 失败。`etcd.enabled=false` 时强制注入 `EDGEFLOW_CLOUDCORE_ETCD_ENABLED=false` 并**忽略 external.***（总开关优先，纯内存逃生）。
- TLS 证书文件建议经 Secret/ConfigMap 挂载到容器的只读路径（distroless 镜像无 shell，路径即注入值）。
- cloudcore 与外部 etcd 的依赖就绪时序：建议 K8s 外部依赖探活（initContainer：`etcdctl endpoint health` 或 nc 探活）或先部署 etcd 再部署本 Chart。
