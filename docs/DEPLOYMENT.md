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
| `cloudcore.replicaCount` | `1` | 副本数；**embed 模式必须 1**（多副本各自 embed 脑裂，模板 `{{ fail }}` 兜底）；**外部模式 v0.6.0 起可 >1**（真多活，自动注入 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`，前置要求见 §10.8.1） |
| `cloudcore.port.http` / `.hub` | `8080` / `10000` | 容器监听端口（与 Dockerfile EXPOSE 对齐） |
| `cloudcore.env` | `EDGEFLOW_CLOUDCORE_PORT=8080`、`EDGEFLOW_CLOUDCORE_HUB_PORT=10000`、`EDGEFLOW_CLOUDCORE_ETCD_*`（v0.4.0，见下方 etcd 段） | 环境变量透传（key/value 形式） |
| `cloudcore.extraEnv` | `[]` | 扩展 env 列表（valueFrom/secretKeyRef 等复杂引用） |
| `cloudcore.etcd.enabled` / `.dataDir` | `true` / `/data/etcd` | v0.4.0 嵌入式 etcd 总开关与数据目录（容器内路径，PVC 挂载 `/data`） |
| `cloudcore.etcd.persistence.enabled` / `.storageClass` / `.size` | `true` / 空（默认 StorageClass） / `1Gi` | 数据卷 PVC 开关/存储类/容量；**Pod 重建不丢数据的前提**（emptyDir 会丢） |
| `cloudcore.etcd.env.*` | `EDGEFLOW_CLOUDCORE_ETCD_*` 全量默认（见 §10.2 配置表） | etcd 配置透传：quota 256MiB / compaction periodic 1h / 回环端口 12379/12380 / STRICT 默认 off |
| `cloudcore.etcd.external.{enabled,endpoints,tls.*,allowInsecure,nodeLeaseTTL}` | `false` / `[]` / 空 / `false` / `""` | v0.5.0 外部 etcd 段（ENDPOINTS/TLS/逃生门注入，不创建 PVC，见 §10.7.7）；**v0.6.0 新增 `nodeLeaseTTL`**（非空注入 `EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL`，默认 300s，见 §10.8.4）；`enabled=true ∧ replicaCount>1` 时自动注入 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`（见 §10.8.2） |
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

- **`replicaCount`（v0.6.0 修订）**：embed 模式（默认）**必须 = 1（硬约束）**——嵌入式 etcd 为单成员；多副本各自 embed 单成员 + 共享 PVC = **脑裂**（各自独立的数据分叉）。Chart 注释已显式禁止，模板 `{{ fail }}` 渲染守卫兜底。**外部模式（external.enabled=true）v0.6.0 起支持 >1**（真多活，见 §10.8）；v0.5.0 的单写者铁律已按 R15 解除（v0.5.0 历史口径见 §10.7.1 ⛔ 段）。
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
| /healthz | 进程存活（不反映 etcd） | **单副本：不反映 etcd**（同左，避免 K8s 批量重启放大故障）；**多副本（MULTI_REPLICA，v0.6.0 起）：反映 etcd 连接**（失联 >TTL → 503 → liveness 重启自愈，见 §10.8.3） |
| 副本数 | **必须 1**（多副本 embed + 共享 PVC = 脑裂） | **v0.6.0 起：可 >1**（真多活：判活 = etcd 租约视角 + 删除守卫，见 §10.8）；v0.5.0 单写者铁律，见下方 ⛔ 段（历史口径） |

**⛔ 单写者形态铁律（v0.5.0 历史口径；v0.6.0 已在外部模式解除，见 R15/§10.8）**：v0.5.0 两种模式都只支持**一个 cloudcore 写入方**。外部模式虽然共享同一键空间（写穿保证 etcd 侧一致、无数据分叉），但**心跳/Offline 判活是各副本内存瞬态**（不落盘）：副本 A 看不到连在副本 B 上的活节点心跳 → 180s+保留期后 A 会把它**从共享键空间判死删除并级联删设备 Desired**——活节点数据丢失且无任何信号。v0.5.0 受支持形态 = 单写者（replicaCount=1 或 active/standby 同一时刻仅一个真在服务）；真多活（etcd lease 心跳）v0.6.0 落地（R15）：心跳落盘为租约、判活 = etcd 视角 hb 键存在性、删除带 GuardedDelete 守卫，多副本对判活与删除均安全。**v0.6.0 起 embed 模式仍必须 1**（脑裂不因代码演进消失）；外部模式多副本要求与配置见 **§10.8**。

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
| 多副本误配 | **v0.6.0 起仅 embed 模式被 `{{ fail }}` 拦截**（embed 多副本 = 脑裂）；外部模式合法（见 §10.8，需满足前置要求）；手工裸跑二进制时，embed/纯内存请保持单副本，外部模式多副本请按 §10.8 检查同版本/quorum/共享 endpoints |

---

### 10.9 模型仓库与灰度发布（v0.7.0）

> 核心能力：云端内置**模型仓库**（模型 + 版本两级台账 + 部署影子）与**灰度发布**（按节点白名单/按比例、分批、fail-fast、取消、回滚）——手册 F41/F42 落地为正式功能。设计见 ARCHITECTURE.md 决策 **R16**；API 契约见 **API-SPEC.md §7**；限制登记见 KNOWN-ISSUES.md §7（L21-L31）。**边缘零代码改动**：发布器复用既有 podsync（镜像 Pod）+ config-sync（模型版本/参数 ConfigMap）经可靠投递下发，旧版 edgecore 直接可用。

#### 10.9.1 三模式行为矩阵

| 能力 | 纯内存（ETCD_ENABLED=false 且无 ENDPOINTS） | embed etcd（默认） | 外部 etcd（多副本） |
|---|---|---|---|
| 模型/版本/发布 CRUD | ✅ 内存（mutex 串行） | ✅ 写穿持久化 | ✅ 写穿 + CAS + watch |
| release 任务执行 | ✅ 正常工作 | ✅ | ✅（领跑锁 + 接管） |
| release/部署影子重启恢复 | ❌ 重启丢失（**L22 登记**，明示） | ✅ etcd 恢复 | ✅ etcd 恢复 |
| 发布领跑锁 | 单实例恒成功（逻辑空转） | 同左 | 租约锁 + 接管（≤TTL 60s） |
| 并发安全 | 单进程 | 单副本 CAS 恒成功（D4 口径） | CAS + guard + 锁 |
| 部署影子恢复 | 重启清空 | 恢复 | 恢复 |

> **模式差异补注**：不同模式/迁移后百分比发布的目标集合**不跨模式可比**——百分比分母 = 创建时刻 Ready 节点（embed/外部模式的 Ready 判定口径不同），且目标集合在创建时物化为快照；以创建时快照为准，跨模式迁移后重新发起发布（审稿线索 4 口径）。

#### 10.9.2 配置（新增 env，全部可选）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_RELEASE_SCAN_INTERVAL` | `5s` | 发布控制器扫描周期（>0，非法 fail-fast，对齐既有 duration env 风格） |
| `EDGEFLOW_CLOUDCORE_RELEASE_LOCK_TTL` | `60s` | 发布领跑锁租约 TTL（**>=15s**，非法 fail-fast）；刷新周期与 TTL 绑定 = `max(5s, TTL/3)`（主线裁决 D5）；**仅外部模式消费**，embed/纯内存显式设置 → Warn 忽略（并入 warnEmbedFieldsIgnored 族） |
| `EDGEFLOW_CLOUDCORE_ETCD_USERNAME` / `EDGEFLOW_CLOUDCORE_ETCD_PASSWORD` | 空 | 外部 etcd RBAC 用户名密码鉴权（v0.8.0，L1）：必须成对设置（只设其一 fail-fast）；与 TLS/mTLS 正交；仅外部模式消费，embed/纯内存显式设置 → Warn 忽略 |
| `EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED` | 空 | 终态发布 GC 开关（v0.8.0，L28）：`1`/`true` 开启；默认关闭（L31 审计口径——终态 release 键永久保留） |
| `EDGEFLOW_CLOUDCORE_RELEASE_GC_KEEP` | `100` | 终态发布保留条数（v0.8.0，L28）：≥1，非法 fail-fast；仅 GC 开启时消费 |
| `EDGEFLOW_CLOUDCORE_RELEASE_MIRROR_CHECK` | 空 | 发布前镜像存在性探活模式（v0.9.0，R-1）：空/off=不检查（默认，零行为变化）、warn=失败仅告警（发布照常）、fail=失败阻断发布（422）；非法值 fail-fast |
| `EDGEFLOW_CLOUDCORE_RELEASE_MIRROR_CHECK_TIMEOUT` | `5s` | 镜像探活超时（>0，非法 fail-fast）；Docker Hub 需先换 token（约 2 次 RTT） |
| `EDGEFLOW_CLOUDCORE_REGISTRY_TOKEN` | 空 | 私有 registry 的 Bearer token（可选；Docker Hub 自动换取无需配置） |

K8s 部署（Helm）：两者均可经 `cloudcore.env` 透传（values.yaml 已留注释行示例）；Chart 无新增 values 必填项。

#### 10.9.3 使用流程（灰度运营）

```bash
# ① 注册模型（模型名唯一；镜像实体在客户镜像仓库，平台登记镜像 ref + sha256 摘要）
curl -X POST http://<cloudcore>:8080/api/v1/models \
  -d '{"name":"defect-detector","description":"缺陷检测模型","type":"detection"}'
# ② 登记版本（"Tag 即版本"；status=draft；sha256 必填防篡改登记）
curl -X POST http://<cloudcore>:8080/api/v1/models/defect-detector/versions \
  -d '{"version":"v1.2.0","mirror":"registry.example.com/edgeflow/models/defect-detector:v1.2.0","sha256":"sha256:9f86...","sizeBytes":482344960,"archs":["amd64","arm64"],"metadata":{"threshold":"0.8"}}'
# ③ 激活版本（draft→active，自动降级旧 active；发布目标必须 active）
curl -X POST http://<cloudcore>:8080/api/v1/models/defect-detector/versions/v1.2.0/activate
# ④ 灰度发布（202 受理）：先 1 台试点（白名单）→ 小批（batchSize+pauseBetween）→ 全量（percentage=100）
curl -X POST http://<cloudcore>:8080/api/v1/models/defect-detector/releases \
  -d '{"version":"v1.2.0","target":{"type":"percentage","percentage":25},"batchSize":2,"pauseBetween":30000,"failFast":true}'
# → 202 + release（status=pending，targetNodes 已物化）
# ⑤ 跟踪/取消/回滚
curl http://<cloudcore>:8080/api/v1/models/defect-detector/releases/<releaseID>   # perNode 汇总
curl -X POST http://<cloudcore>:8080/api/v1/models/defect-detector/releases/<releaseID>/cancel
curl -X POST http://<cloudcore>:8080/api/v1/models/defect-detector/releases/<releaseID>/rollback   # 202，逆序回滚
curl http://<cloudcore>:8080/api/v1/models/defect-detector/deployments            # 版本—节点—时间台账
```

**灰度运营建议**：

1. **节奏：先 1 台试点 → 小批 → 全量**。试点用白名单（`target.type=nodeIDs`，1 台）验证镜像可用与推理结果；小批用 `batchSize=1~2 + pauseBetween ≥30s` 留观察窗口；验证通过后放大比例或白名单；确认无误后 100%。
2. **fail-fast 保持默认开（true）**：单节点失败立即中止并标记剩余节点 skipped，避免坏版本扩散；需要"看完全部节点失败情况"时再显式关。
3. **回滚前置条件 = "该发布版本仍是模型当前 active 版本"**（未被新版本接管）；已被接管 → 409，先显式 activate 目标旧版本（或发起新发布）再回滚。回滚为逆序批量执行、失败不回滚中止（能回多少回多少，perNode 明细可查）。
4. **发布成功 ≠ 镜像可用**：平台下发的是声明（mirror ref），拉取发生在边缘；镜像仓库不可达/镜像损坏会在边缘 PodStatus 暴露（CrashLoop 等），发布任务本身仍可能 succeeded——发布前建议在试点节点确认 Pod Running。
5. **耗时口径**：批内逐节点**串行**（每节点 2 次可靠下发，最坏 ~10s+/节点超时）；batchSize 控制批粒度/暂停节奏，**不是并发度**；大 fleet 全量耗时 = Σ(单节点部署耗时)+Σ(pause)（D6 口径）。
6. **模型实例多副本**：发布语义 = "该版本上机"（replicas=1）；多副本由用户后续自行 podsync 编排。

#### 10.9.4 升级 / 回滚（v0.6.0 ↔ v0.7.0）

- **升级（v0.6.0 → v0.7.0）**：全停再全起（惯例，`kubectl scale deploy <rel>-cloudcore --replicas=0` → 换 v0.7.0 镜像 → 起 1 副本验证 → 外部模式再扩容）。零迁移动作：v0.7.0 只新增 `/edgeflow/models/` 前缀，既有键/JSON 逐字不变（schemaVersion 不 bump）；既有 14 端点行为逐字节不变（回归锚点）。
- **回滚（v0.7.0 → v0.6.0）**：全停 → 部署 v0.6.0（replicas=1）。残留 `/edgeflow/models/` 键**无害**（v0.6.0 不读不写该前缀）；可选显式清理：`etcdctl del /edgeflow/models --prefix`（外部模式同法；embed 模式客户端端口 127.0.0.1:12379）。
- **混合版本多副本未验证（L29）**：v0.6.0 与 v0.7.0 副本同连一集群未验证；理论无冲突（新前缀隔离、旧版不读新键）仍**建议同版本全量切换**。

#### 10.7.7 Helm 部署（外部模式）

```yaml
cloudcore:
  replicaCount: 1          # 单副本：healthz 保持进程存活语义；多副本（v0.6.0 真多活）见 §10.8
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
      nodeLeaseTTL: ""      # v0.6.0：非空注入 EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL（默认 300s，见 §10.8.4）
    persistence:            # 外部模式不落本地盘：PVC 自动跳过、/data 回退 emptyDir
      enabled: true         # （保持 true 也不创建 PVC）
```

- 模板行为：`external.enabled=true` → 注入 `EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS`（endpoints 逗号连接）+ TLS/逃生门 env；**不注入** `ETCD_DATA_DIR` 等 embed env；**不创建 PVC**。
- 渲染守卫（`{{ fail }}`）：**仅 embed 模式** `replicaCount>1` → 失败（脑裂；v0.6.0 起外部模式放行）；`external.enabled=true` 且 `endpoints` 为空 → 失败。`etcd.enabled=false` 时强制注入 `EDGEFLOW_CLOUDCORE_ETCD_ENABLED=false` 并**忽略 external.***（总开关优先，纯内存逃生）。
- **v0.6.0 多副本注入**：`external.enabled=true` ∧ `replicaCount>1` → 自动注入 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`（/healthz 绑定 etcd，见 §10.8.3）；`nodeLeaseTTL` 非空 → 注入 `EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL`。
- TLS 证书文件建议经 Secret/ConfigMap 挂载到容器的只读路径（distroless 镜像无 shell，路径即注入值）。
- cloudcore 与外部 etcd 的依赖就绪时序：建议 K8s 外部依赖探活（initContainer：`etcdctl endpoint health` 或 nc 探活）或先部署 etcd 再部署本 Chart。


---

### 10.8 多副本部署指南（v0.6.0 真多活，外部模式）

> 核心能力：外部 etcd 模式下 cloudcore 支持 `replicaCount>1` 的 active-active 多副本（**真多活**）——心跳落盘为 etcd 租约（`/edgeflow/registry/heartbeats/<nodeID>`），判活 = etcd 视角的 hb 键存在性（所有副本同一事实源），删除带 GuardedDelete 守卫（防误删活节点），读一致 = Load 锚定 + watch 增量 + 重扫兜底。设计见 ARCHITECTURE.md 决策 **R15**；限制登记见 KNOWN-ISSUES.md §6（L12-L20）。**embed 模式多副本仍显式禁止（脑裂），本节只适用于外部模式。**

#### 10.8.1 前置要求（不满足即数据风险）

1. **外部 etcd 集群 3 节点、quorum 健康**（§10.7.4 基线：2/3 法定人数、同地域、quota 256MiB/compaction 1h、TLS 或 mTLS）。**判活依赖 etcd 可用性**（KNOWN-ISSUES L12）：quorum 丢失/全断 > lease TTL（默认 300s）时节点会**全量软离线**，恢复后数分钟内自愈、**零数据删除**——这是相对 v0.5.0「判活不受存储故障影响」的语义变化，监控告警阈值按 TTL 折算（见 §10.8.4）。
2. **全部副本同版本，禁止新旧混跑**（KNOWN-ISSUES L15）：v0.5.0 与 v0.6.0 cloudcore 副本同连一个集群 = 旧版本无心跳键视角，其 GC 会把存活的节点**误删**（数据丢失）。升级/回滚必须「全停再全起」（scale 0 → 1，见 §10.8.5/§10.8.6）。
3. **fleet 共享同一 endpoints**：cloudcore 无状态、无选主、无逐副本差异化 env（副本对称）；各副本服务不同边缘子集或由负载均衡打散均可（CAS + 删除守卫使正确性不依赖粘性路由，粘性仅为体验建议）。

#### 10.8.2 配置（replicaCount>1）

```yaml
cloudcore:
  replicaCount: 2            # v0.6.0 起外部模式支持 >1
  etcd:
    enabled: true
    external:
      enabled: true
      endpoints:             # fleet 共享同一列表（clientv3 自动 failover）
        - http://10.0.0.11:2379
        - http://10.0.0.12:2379
        - http://10.0.0.13:2379
      tls:
        ca: /etc/edgeflow/etcd-tls/ca.crt
        cert: /etc/edgeflow/etcd-tls/tls.crt
        key: /etc/edgeflow/etcd-tls/tls.key
      # nodeLeaseTTL: "300s"   # 可选：心跳租约 TTL（默认 300s，调参见 §10.8.4）
```

- 模板自动行为：`external.enabled=true` ∧ `replicaCount>1` → 注入 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`（healthz 绑定，见 §10.8.3）；`nodeLeaseTTL` 非空 → 注入 `EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL`。
- 手动裸跑二进制（非 Helm）时，需自行设置 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`（"1"/"true" 生效）——该 env 是纯环境变量派生，Chart 注入只是自动化的便捷。
- 缩容/扩容副本数随时可做（各副本对称、启动即 Load 追平 + 续约接管），无需维护排他状态。

#### 10.8.3 healthz / liveness 语义（多副本绑定）

| 形态 | /healthz 语义 | 说明 |
|------|--------------|------|
| embed 模式（任何副本数=1） | 进程存活（恒 200） | v0.5.0 语义不变 |
| 外部模式 + 单副本（无 MULTI_REPLICA） | 进程存活（恒 200） | v0.5.0 语义不变（避免 K8s 批量重启放大故障——单副本重启即全量断连） |
| 外部模式 + 多副本（MULTI_REPLICA=1） | **进程存活 + etcd 连接**：周期探活/续约成功率，失联 >TTL → **503** | K8s liveness 判定失败 → 重启该副本 → 自愈（收敛「边连接面分叉」残余窗口：A 与 etcd 分区但仍在服务边缘时，重启 A 即可） |

- liveness 探针保持默认（path `/healthz`，failureThreshold 3 × period 10s）；多副本形态下 503 只重启故障副本，其余副本正常服务。
- 残余窗口记录：多副本形态下若某副本与 etcd 分区但继续服务其边缘连接（心跳无法续约），其他副本会按 TTL 将该批节点判离线——该副本被 liveness 重启后收敛；窗口内跨副本读可能不一致（KNOWN-ISSUES L12/L13）。

#### 10.8.4 NODE_LEASE_TTL 调参权衡（故障免疫 vs 检测延迟）

`EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL`（Helm：`cloudcore.etcd.external.nodeLeaseTTL`）是心跳租约 TTL = 外部模式判活阈值。**默认 300s（5m）= 10× 边缘心跳周期 30s**；校验：<90s Warn（低于心跳周期 3 倍，抖动误判风险）；≤0/非法 fail-fast；仅外部模式消费（embed/纯内存显式设置 → 启动期 Warn 忽略，不阻断）。

| TTL | 故障免疫（etcd 故障期间判活不波动） | 离线检测时延上界（断开事件丢失场景） | 备注 |
|-----|-----------------------------------|-----------------------------------|------|
| 90s（下限，Warn 边界） | 90s | ≈ 2×TTL = 180s | 抖动误判风险高，仅测试/短故障窗口场景 |
| 180s（v0.5.0 NodeController 阈值同值） | 3m | ≈ 6m | 与 v0.5.0 判活阈值同量级，但故障免疫从「无限」塌缩为 3m |
| **300s（默认）** | **5m** | **≈ 10m** | 覆盖：CloudHub 90s 断开事件 + 边缘重连上限 60s + 副本重启/迁移预算 ~2m + 常见 quorum 恢复窗口 |
| 600s | 10m | ≈ 20m | 长故障窗口场景：降低假离线，牺牲离线检出时延（孤儿台账保留期不变，仍 24h） |

- **检测延迟语义**：正常断开仍由 CloudHub 90s 断开事件**快路径**判离线（不变）；lease 过期只兜「断开事件丢失/副本死亡」场景。监控告警阈值按上表「时延上界」折算（L13b）。
- **`NODE_RETENTION` 24h 不变**：lease TTL 只是「hb 键寿命」= 软离线判定；台账键不绑租约，24h 保留期（Offline 展示 + GC 门槛）与 v0.5.0 完全一致。
- 语义迁移（L20）：外部模式下 `NODE_SCAN_INTERVAL` = etcd 重扫/GC 周期（不变，语义迁用）；**`NODE_TIMEOUT` 不再作为外部模式判活阈值**（NodeController 停用，判活阈值 = `NODE_LEASE_TTL` 独立默认 300s——主线裁决 D2 与 NODE_TIMEOUT 解耦）；embed/纯内存两 env 语义不变（NodeController 扫描周期/阈值）。

#### 10.8.5 升级（v0.5.0 单写者 → v0.6.0 多副本）

**零迁移动作**：台账 JSON、设备 Desired JSON、既有键空间全部兼容（v0.6.0 只新增 `/edgeflow/registry/heartbeats/` 前缀；v0.5.0 进程不读该前缀）。唯一要求：**全停再全起，禁止混跑**（L15）。

```bash
# ① 停旧（关键！防混跑）
kubectl scale deploy <rel>-cloudcore --replicas=0
# ② 换镜像部署 v0.6.0（replicas=1）
helm upgrade <rel> . --set cloudcore.image.tag=v0.6.0 \
  --set cloudcore.etcd.external.enabled=true --set <endpoints...>
# ③ 验证：节点注册回（≤重连+心跳周期）、台账/Desired 完整（Load 全量恢复）、
#    etcdctl get /edgeflow/registry/heartbeats/ --prefix 出现租约键
# ④ 扩容多副本（可选）
kubectl scale deploy <rel>-cloudcore --replicas=2
# ⑤ 验证双副本：另一副本 /api/v1/nodes 可见同样节点且 Ready（≤2×扫描周期 + watch 余量）
```

- 升级窗口内的判活空窗：旧实例停 → 新实例 Load 完成期间节点短暂 Unknown/Offline（租约 TTL 内自动恢复，≤300s），与 v0.5.0 崩溃恢复式故障转移同口径。
- 遗留：v0.5.0 期孤儿台账键（L7 时代产物）→ v0.6.0 Load 后按 Unknown + 保留期 GC 正常清理（自愈）。

#### 10.8.6 回滚（v0.6.0 → v0.5.0）

```bash
# ① 停 v0.6.0（全部副本）
kubectl scale deploy <rel>-cloudcore --replicas=0
# ② （可选但推荐）清理 lease 键空间——心跳键独立前缀，v0.5.0 的 Load 前缀扫描扫不到，
#    残留零脏键、零告警；显式清理可让键空间与 v0.5.0 完全同构：
etcdctl del /edgeflow/registry/heartbeats --prefix
# ③ 部署 v0.5.0（replicas=1）
# ④ 验证：台账/Desired 完整（v0.6.0 未改格式）；节点经重注册/心跳翻新为 Ready
```

- **双向断言**：v0.6.0 写的数据 v0.5.0 可读（JSON 逐字段兼容）；v0.5.0 写的数据 v0.6.0 可读（无新必填字段）。schemaVersion 不 bump（业务键形态未变，新增键空间属向后兼容扩展）。
- **禁止**：v0.6.0 与 v0.5.0 副本同时运行（L15）。

#### 10.8.7 排障

| 现象 | 排查 |
|------|------|
| **全量软离线**（所有节点短时 Offline，随后自动恢复 Ready） | etcd 故障窗口 > lease TTL 的预期行为（L12）：quorum 丢失/全断期间租约不续 → 到期删 hb 键 → 判离线；**etcd 恢复后 ≤1 心跳周期自动重建租约自愈，零数据删除**（台账 24h 保留 + GuardedDelete 守卫）。确认：`etcdctl endpoint health`；观察「续约失败」日志；如需更长免疫窗口调大 TTL（§10.8.4）。若故障 <TTL，应有续约重试缓冲、节点不判离线 |
| 节点真正断开但迟迟不判 Offline | 断开事件丢失场景下，判离线依赖租约到期：时延上界 ≈ 2×TTL（L13b）；要更快可调小 TTL（权衡免疫）或确认 CloudHub 90s 断开事件在服务 |
| 日志出现 `concurrent-write`（SetDesired 冲突重试耗尽） | **预期语义，非错误**：多副本并发对同一设备写 Desired 时 CAS 冲突；重试 ≤3 次后返回 error、**HTTP 仍 200**（指令已 Ack 到边缘），Desired 未更新，凭日志（含 nodeID/device/property）重发指令或等下次下发收敛（见 API-SPEC 设备指令语义） |
| 单副本误设 MULTI_REPLICA | 不推荐（healthz 绑定 etcd 后，etcd 故障窗口会触发自身重启放大影响）；Helm 只在 replicaCount>1 时注入，手工裸跑请勿单副本设置 |
| 多副本下某副本与 etcd 分区但边缘连接正常 | 该副本无法续约 → 其节点 ≤TTL 判离线（etcd 视角正确）；读路径仍服务自身内存（可能陈旧）；**liveness（healthz 503）会重启该副本自愈**（§10.8.3 残余窗口） |
| embed 模式 replicaCount>1 | 渲染期 `{{ fail }}` 拦截（脑裂文案）；手工裸跑请保持 1 |
