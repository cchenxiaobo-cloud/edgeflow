# keadm 使用文档（WBS 8.6 基础版 + 升级回滚）

`keadm` 是 EdgeFlow 的安装管理 CLI（对标 KubeEdge 的 keadm）。当前为**基础版 + 升级回滚**：
只做**离线产物生成**（不直接操作集群/节点），生成物由用户拿到真实集群/边缘节点上执行；
`keadm upgrade` / `rollback` 在产物层面提供升级与回滚（备份模型 + 操作台账，见 docs/UPGRADE.md）。

- 云端：生成可直接 `kubectl apply -f` 的 `cloudcore.yaml`（Deployment + Service），
  与 `build/charts/edgeflow` 的容器约定完全一致（`/healthz` 探针、`/data` 卷、TLS env 透传）；
  另输出 `NOTES.txt` 给出 Helm Chart 替代路径。
- 边缘：生成 `edgecore.env`（环境变量文件）+ `edgecore.service`（systemd 单元）+
  `install.sh`（安装脚本）+ `README.md`（接入说明）。

## 构建

```bash
# 本地构建
go build -o bin/keadm ./cmd/keadm

# 交叉编译（keadm 本身在管理机上运行，通常无需交叉编译）
GOOS=linux GOARCH=amd64 go build -o keadm-linux-amd64 ./cmd/keadm
```

版本信息通过 `-ldflags` 注入（与 cloudcore/edgecore 一致，见 Makefile）：

```bash
go build -ldflags "-X edgeflow/pkg/version.Version=v0.1.0 \
  -X edgeflow/pkg/version.GitCommit=$(git rev-parse --short HEAD) \
  -X edgeflow/pkg/version.BuildTime=$(date +%Y-%m-%dT%H:%M:%S%z)" -o bin/keadm ./cmd/keadm
```

## 命令总览

| 命令 | 用途 | 产物 |
| --- | --- | --- |
| `keadm init` | 生成云端部署产物 | `cloudcore.yaml`、`NOTES.txt` |
| `keadm join` | 生成边缘接入产物 | `edgecore.env`、`edgecore.service`、`install.sh`、`README.md` |
| `keadm upgrade` | 产物升级（先备份 + 写操作台账） | `backups/<id>/`、`ops-ledger.jsonl` |
| `keadm rollback` | 产物回滚（从备份恢复，事务化） | 恢复产物文件 |
| `keadm ops-ledger` | 查询操作台账（时间/版本/结果/操作人） | — |
| `keadm reset` | 清理 output-dir 下的 keadm 生成产物（确认后删除，幂等） | — |
| `keadm version` | 输出版本信息（`--json` 结构化输出） | — |

> 升级/回滚为 M4-M5 补充能力（commits `7aa035c`/`fe093e1`，WBS 10.2）；完整机制说明与
> 演练记录见 docs/UPGRADE.md。批量 init/join 与跨版本配置迁移未实现（见 audit-m35 G8）。

退出码约定：`0` 成功；`1` 运行时错误（IO/生成失败）；`2` 参数/用法错误。

## 1. 初始化云端（keadm init）

```bash
# 最简（明文通道，本地/测试用）
keadm init --output-dir=./keadm-out

# 生产推荐：启用云边 mTLS，并注入证书 SAN（边缘节点用 IP 接入时必须覆盖访问地址）
keadm init --tls --tls-san=IP:192.168.1.10 --output-dir=./keadm-out
```

参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--cloudcore-image` | `edgeflow/cloudcore:v0.1.0` | cloudcore 容器镜像 |
| `--tls` | 关 | 启用 mTLS：注入 `EDGEFLOW_CLOUDCORE_TLS=on` + `EDGEFLOW_CLOUDCORE_CERT_DIR=/data/certs` |
| `--tls-san` | 空 | 证书 SAN（逗号分隔，如 `IP:1.2.3.4,DNS:edge.example.com`），仅 `--tls` 时生效 |
| `--service-type` | `NodePort` | Service 类型：`NodePort`（边缘跨集群接入）或 `ClusterIP`（仅集群内访问） |
| `--output-dir` | `./keadm-out` | 产物输出目录 |

产物内容（与 Chart 约定对齐）：

- **Deployment** `edgeflow-cloudcore`：副本 1、`/data` emptyDir 卷、
  `livenessProbe`/`readinessProbe` 均探 `/healthz`（http 8080）、
  nonroot（65532）+ 只读根文件系统安全上下文、TLS env 透传。
- **Service** `edgeflow-cloudcore`：`NodePort`，`http:8080` + `hub:10000`
  （hub 端口 NodePort 由集群自动分配在 30000-32767）。
- **NOTES.txt**：部署步骤、验证方法、Helm 替代路径、mTLS 说明。

### 在真实集群上执行

```bash
# 1. 应用产物
kubectl apply -f cloudcore.yaml

# 2. 验证
kubectl get deploy,svc,pods -l app.kubernetes.io/component=cloudcore
kubectl port-forward svc/edgeflow-cloudcore 8080:8080
curl http://127.0.0.1:8080/healthz        # 期望 HTTP 200

# 3. 获取边缘节点接入用的 CloudHub 节点端口
kubectl get svc edgeflow-cloudcore -o jsonpath='{.spec.ports[?(@.name=="hub")].nodePort}'
```

Helm 替代路径（功能等价，见 `NOTES.txt`）：

```bash
helm install edgeflow build/charts/edgeflow \
  --set service.hubEnabled=true --set service.type=NodePort \
  --set cloudcore.env.EDGEFLOW_CLOUDCORE_TLS=on \
  --set cloudcore.env.EDGEFLOW_CLOUDCORE_CERT_DIR=/data/certs
```

## 2. 边缘节点接入（keadm join）

```bash
# 在管理机/边缘节点生成接入产物
keadm join --cloudcore-ip=192.168.1.10 --token=<token> --output-dir=./keadm-out

# TLS 集群 + NodePort 部署（端口为集群分配的 hub 节点端口）
keadm join --cloudcore-ip=192.168.1.10 --cloudcore-port=31000 \
  --token=<token> --node-id=edge-worker-01 --tls --output-dir=./keadm-out
```

参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--cloudcore-ip` | 必填 | 云端 CloudHub 节点 IP（IPv4/IPv6 均可，IPv6 自动加方括号） |
| `--cloudcore-port` | `10000` | CloudHub 端口；NodePort 部署时填集群分配的节点端口 |
| `--token` | 必填 | 接入令牌（预留字段：当前 edgecore 尚未消费，后续版本用于接入鉴权） |
| `--node-id` | `edge-<主机名>` | 边缘节点 ID（与 edgehub 默认命名约定一致） |
| `--tls` | 关 | 启用 mTLS：注入 `EDGEFLOW_EDGECORE_TLS=on` + `CERT_DIR`，地址用 `wss://` |
| `--output-dir` | `./keadm-out` | 产物输出目录 |

产物内容：

- **edgecore.env**：`EDGEFLOW_EDGECORE_NODE_ID` / `CLOUD_ADDR`（`ws://<ip>:<port>/v1/edge`）/
  `DB_PATH`（`/var/lib/edgeflow/edgeflow.db`，绝对路径避免 systemd 相对 cwd 歧义）/
  `MQTT_ADDR`（`tcp://127.0.0.1:1883`）/ `TLS`+`CERT_DIR`（`--tls` 时）/
  `TOKEN`（预留）。键名与 edgecore 读取的环境变量一一对应。
- **edgecore.service**：systemd 单元，`EnvironmentFile=/etc/edgeflow/edgecore.env`、
  `ExecStart=/usr/local/bin/edgecore`、`WorkingDirectory=/var/lib/edgeflow`、
  `Restart=on-failure`、`WantedBy=multi-user.target`。
- **install.sh**：一键安装脚本（需 root），安装二进制 + env + 单元并 `systemctl enable --now`。
- **README.md**：接入说明与手动安装片段。

### 在真实边缘节点上执行

```bash
# 1. 准备 edgecore 二进制（与节点架构匹配）
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o edgecore ./cmd/edgecore

# 2. 拷贝产物 + 二进制到边缘节点，然后执行
sudo ./install.sh

# 3. 验证
systemctl status edgecore
journalctl -u edgecore -f
```

mTLS 说明：edgecore 首次运行会在 `/etc/edgeflow/certs` 自动生成/加载 CA 与
客户端证书（幂等），客户端证书 CN 按约定为 `edgeflow-<nodeID>`；云端认证依据是
CA 签名。云端侧需保证服务端证书 SAN 覆盖边缘节点访问的地址
（`keadm init --tls-san=IP:<ip>`）。

## 3. 清理（keadm reset）

```bash
# 交互确认后删除 output-dir 下的 keadm 生成产物
keadm reset --output-dir=./keadm-out

# 跳过确认（脚本/CI 使用）
keadm reset --force --output-dir=./keadm-out
```

行为说明：

- **只清理 keadm 生成的文件**（init/join 产物清单），目录中用户自己的文件一律不动；
- **幂等**：目录不存在或没有 keadm 产物时提示后以 `0` 退出；
- 删除后目录若为空则一并移除；`--force` 跳过确认提示。

## 4. 版本（keadm version）

```bash
keadm version          # keadm version=v0.1.0 gitCommit=... buildTime=... goVersion=...
keadm version --json   # 结构化 JSON，供脚本解析
```

## 5. 异常排障

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `缺少必填参数 --cloudcore-ip` | join 未传 IP | 补 `--cloudcore-ip=<ip>` |
| `--cloudcore-ip="..." 不是合法 IP` | IP 格式错误（如 `999.1.1.1`、域名） | 使用合法 IPv4/IPv6；域名支持属后续版本 |
| `缺少必填参数 --token` | join 未传令牌 | 补 `--token=<token>`（云端签发/预共享） |
| `--node-id 含空白字符` | 节点 ID 带空格 | 用 `--node-id=edge-xxx` 显式指定 |
| `--cloudcore-image 不能为空` | init 传空镜像 | 使用默认值或合法镜像名 |
| `未知子命令` / 无参数 | 用法错误（退出码 2） | `keadm --help` 查看用法 |
| `kubectl apply` 报 schema 校验失败 | 集群 API 版本与本地 kubectl 不匹配 | 检查 kubectl 版本与集群兼容性 |
| edgecore 起不来 | 网络不通/配置错误 | `journalctl -u edgecore -e` 查看；确认可达 `--cloudcore-ip` 的 hub 端口；确认 env 文件键值未被手改 |
| 云边连接被拒（TLS） | 证书 SAN 未覆盖访问地址 | 云端以 `--tls-san=IP:<访问IP>` 重新 init 并 apply |
| 升级/回滚异常 | 备份缺失/校验失败/中途失败 | 见 docs/UPGRADE.md §3 异常路径表（含人工 cp 兜底命令） |

## 5.1 升级与回滚（产物层面）

```bash
# 升级产物到新版本（执行前自动备份 + 写台账）
keadm upgrade --version=v0.2.0 --operator=alice --output-dir=./keadm-out

# 模拟失败演练（不改动产物，验证异常路径）
keadm upgrade --version=v0.2.0 --simulate-failure --output-dir=./keadm-out

# 回滚到最近一次备份（staging 校验通过后原子替换）
keadm rollback --latest --output-dir=./keadm-out

# 查询操作台账
keadm ops-ledger --limit=10
```

机制要点（详见 docs/UPGRADE.md）：备份模型 `backups/<ts>/manifest.json+sha256`；
回滚仅恢复白名单文件（env/service/install.sh），事务化 restore（staging + 原子替换）；
`--simulate-failure` 演练路径与真实失败行为一致。

## 6. 本机验证边界（无集群环境）

本仓库开发环境无 Kubernetes 集群，keadm 的「干净集群安装」验收路径为：

1. **命令级验证**：所有子命令参数校验、退出码、错误提示（`go test ./cmd/keadm`）；
2. **生成物验证**：`cloudcore.yaml` 结构校验（Service NodePort + Deployment
   镜像/探针/卷/TLS env 断言）、`edgecore.env` 键值校验、systemd 单元与安装脚本
   语法检查（`bash -n`）；
3. **真实集群执行**：按上文「在真实集群上执行 / 在真实边缘节点上执行」步骤进行，
   涉及 `kubectl apply`、`systemctl` 的操作需在真实环境完成。
