# EdgeFlow 部署文档（WBS 8.5：镜像构建 + Helm Chart）

本文档覆盖 EdgeFlow 的镜像构建（Docker）与 Kubernetes 部署（Helm Chart），
并说明与 mTLS、配置体系的衔接方式。

- 组件：`cloudcore`（云端）、`edgecore`（边缘端）
- Chart：`build/charts/edgeflow`（Chart 版本 0.1.0 / 应用版本 v0.1.0）
- Dockerfile：`build/docker/Dockerfile`

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
| 数据 | edgecore 的 SQLite 写入 `/data`（已预授权 nonroot） | 生产建议挂载持久卷 |

### 1.5 推送镜像

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
| `cloudcore.replicaCount` | `1` | 副本数；云边为长连接，多副本需会话粘性支持 |
| `cloudcore.port.http` / `.hub` | `8080` / `10000` | 容器监听端口（与 Dockerfile EXPOSE 对齐） |
| `cloudcore.env` | `EDGEFLOW_CLOUDCORE_PORT=8080`、`EDGEFLOW_CLOUDCORE_HUB_PORT=10000` | 环境变量透传（key/value 形式） |
| `cloudcore.extraEnv` | `[]` | 扩展 env 列表（valueFrom/secretKeyRef 等复杂引用） |
| `cloudcore.livenessProbe` / `.readinessProbe` | 10s/5s 起检，间隔 10s/5s | 探针路径固定 `/healthz`（端口按名称 `http` 引用） |
| `cloudcore.resources` | requests 100m/128Mi；limits 500m/512Mi | 保守值，生产按压测调整 |
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
   export EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://<节点IP>:<NodePort>/edgehub
   export EDGEFLOW_EDGECORE_NODE_ID=edge-node-1
   ./bin/edgecore
   ```

   或容器方式（挂载持久卷给 `/data`）：

   ```bash
   docker run -d --name edgecore \
     -e EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://<节点IP>:<NodePort>/edgehub \
     -e EDGEFLOW_EDGECORE_NODE_ID=edge-node-1 \
     -v /var/lib/edgeflow:/data \
     edgeflow/edgecore:v0.1.0
   ```

---

## 3. 与 mTLS / 配置体系的衔接

- **配置优先级**：命令行 `--port` > 环境变量 `EDGEFLOW_CLOUDCORE_PORT` > 配置文件 `config/cloudcore.json` > 默认值。
  容器内默认不内置配置文件，通过 Chart 的 `cloudcore.env` 透传环境变量即可完成端口等配置；
  需要更复杂配置时，可将配置文件以 ConfigMap 挂载并用 `--config` 覆盖（`extraEnv` 或 args 传入）。
- **mTLS（并行任务）**：CloudHub（10000 端口）启用 mTLS 后：
  - 证书可通过 `cloudcore.extraEnv`（如 `EDGEFLOW_CLOUDCORE_TLS_CERT/KEY` 指向挂载路径）注入；
  - 证书文件以 Secret 挂载（`volumeMounts` + `volumes`，Chart 预留了 `extraEnv` 扩展位，
    卷挂载可按需在模板中追加，或后续版本内置 `cloudcore.tls` 字段）；
  - edgecore 侧 `EDGEFLOW_EDGECORE_CLOUD_ADDR` 相应改为 `wss://...` 并配置客户端证书。
- **edgecore 数据**：SQLite 元数据库默认写 `data/edgeflow.db`（容器内为 `/data/edgeflow.db`），
  通过 `EDGEFLOW_EDGECORE_DB_PATH` 可重定向，生产务必挂载持久卷。

---

## 4. 升级 / 回滚

```bash
# 升级（install 与 upgrade 通用，幂等）
helm upgrade --install edgeflow build/charts/edgeflow -f values-prod.yaml

# 查看发布历史
helm history edgeflow

# 回滚到指定版本（REVISION 以 helm history 输出为准）
helm rollback edgeflow 1

# 卸载（注意：卸载不删除 PVC 等持久资源）
helm uninstall edgeflow
```

升级注意：
- Chart 版本变化（`Chart.yaml version` 递增）时使用新 Chart 目录重新 `helm upgrade`；
- 镜像变更走 `--set cloudcore.image.tag=...` 或更新 values 文件；
- 回滚仅恢复 Helm 管理的资源，不恢复数据（数据库在 PVC 中）。

---

## 5. 故障排查

| 现象 | 排查 |
| --- | --- |
| Pod 一直未 Ready | `kubectl describe pod` 看探针结果；确认 `/healthz` 返回 200 |
| ImagePullBackOff | 镜像未推送/私有仓库未配置 `imagePullSecrets`；本机验证用 `pullPolicy: IfNotPresent` + 已构建镜像 |
| 容器内无法 exec | distroless 无 shell，用 `kubectl logs` / `docker cp` 拉日志 |
| 边缘节点连不上 | 确认 Service 的 hub 端口已暴露（`hubEnabled=true`）、防火墙放行、`EDGEFLOW_EDGECORE_CLOUD_ADDR` 协议与端口正确 |
| 权限错误 | distroless nonroot（uid 65532）需要挂载卷可写：emptyDir 默认可写；PV 需配置 fsGroup 或属主 65532 |

---

## 6. 验证清单（本任务已执行）

- [x] `docker build --target cloudcore` / `--target edgecore` 成功（Docker 29.4.3）
- [x] `docker run --rm edgeflow/cloudcore:v0.1.0 --version` 输出 `version=v0.1.0 ... goVersion=go1.26.6`
- [x] 容器冒烟：`/healthz` 返回 200 + `{"status":"ok",...}`；CloudHub 监听 10000
- [x] `helm lint build/charts/edgeflow` 通过（0 failed）
- [x] `helm template` 渲染检查：image / 探针 / env / 资源齐全
- [x] `helm install --dry-run=client` 通过（无需真实集群）
