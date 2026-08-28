# keadm 使用文档（WBS 8.6 基础版 + 升级回滚）

`keadm` 是 EdgeFlow 的安装管理 CLI（对标 KubeEdge 的 keadm）。当前为**基础版 + 升级回滚 + 证书轮换**：
只做**离线产物生成**（不直接操作集群/节点），生成物由用户拿到真实集群/边缘节点上执行；
`keadm upgrade` / `rollback` 在产物层面提供升级与回滚（备份模型 + 操作台账，见 docs/UPGRADE.md），
`keadm cert rotate` 在证书目录层面提供节点证书轮换（备份先行 + 事务化重签，见 §5.2）。

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
| `keadm cert rotate` | 重新签发节点证书（先备份 + 事务化重签） | `backups/<id>/`（旧证书备份） |
| `keadm upgrade` | 产物升级（先备份 + 写操作台账） | `backups/<id>/`、`ops-ledger.jsonl` |
| `keadm rollback` | 产物回滚（从备份恢复，事务化） | 恢复产物文件 |
| `keadm ops-ledger` | 查询操作台账（时间/版本/结果/操作人） | — |
| `keadm reset` | 清理 output-dir 下的 keadm 生成产物（确认后删除，幂等） | — |
| `keadm version` | 输出版本信息（`--json` 结构化输出） | — |

> 升级/回滚为 M4-M5 补充能力（commits `7aa035c`/`fe093e1`，WBS 10.2）；完整机制说明与
> 演练记录见 docs/UPGRADE.md。批量 init 与跨版本配置迁移未实现（批量 join/upgrade/rollback 已实现，见 §5.1；audit-m35 G8）。

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
| `--token` | 必填 | 接入令牌（edgecore 注册携带，云端 EDGEFLOW_CLOUDCORE_NODE_TOKEN 启用时常数时间校验，WBS 7.3） |
| `--node-id` | `edge-<主机名>` | 边缘节点 ID（与 edgehub 默认命名约定一致） |
| `--tls` | 关 | 启用 mTLS：注入 `EDGEFLOW_EDGECORE_TLS=on` + `CERT_DIR`，地址用 `wss://` |
| `--output-dir` | `./keadm-out` | 产物输出目录 |

产物内容：

- **edgecore.env**：`EDGEFLOW_EDGECORE_NODE_ID` / `CLOUD_ADDR`（`ws://<ip>:<port>/v1/edge`）/
  `DB_PATH`（`/var/lib/edgeflow/edgeflow.db`，绝对路径避免 systemd 相对 cwd 歧义）/
  `MQTT_ADDR`（`tcp://127.0.0.1:1883`）/ `TLS`+`CERT_DIR`（`--tls` 时）/
  `TOKEN`。键名与 edgecore 读取的环境变量一一对应（WBS 7.3 设备认证）。
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

### 2.1 接入令牌的安全传递（SEC-03，v0.23.0 起）

`--token` 会经命令行参数传递 token：`ps`/`/proc/<pid>/cmdline` 可见、shell
history 留存，同机其他用户可据此伪造节点注册。生产环境推荐改用文件方式：

```bash
# 令牌写入仅所有者可读的文件（例）
umask 077 && echo '<token>' > ./edgeflow-token.txt

# join：从文件读 token（去首尾空白），产物与 --token 方式完全一致
keadm join --cloudcore-ip=192.168.1.10 --token-file=./edgeflow-token.txt \
  --node-id=edge-worker-01 --output-dir=./keadm-out

# batch join：同样支持（透传到每个节点的 join）
keadm batch --op=join --file=nodes.txt --cloudcore-ip=192.168.1.10 \
  --token-file=./edgeflow-token.txt

# 用后清理
rm -f ./edgeflow-token.txt
```

规则与建议：

- `--token-file` 与 `--token` 同时提供时，**`--token-file` 优先**
  （显式选择了更安全的传参方式），`--token` 值被忽略；
- 令牌文件权限建议 `0600`（所有者可读写）；内容去首尾空白后作为 token，
  空文件视同未提供；
- 产物目录中的 `edgecore.env`（含 token 明文）固定以 `0600` 写入；
  `README.md` 已脱敏（不回显完整 token，明文仅存于 `edgecore.env`）；
- 交互式场景可结合 shell 读入：`keadm join ... --token-file=<(read -s t; echo "$t")`
  （bash process substitution，避免 token 进 history）。

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
| 证书即将到期/需轮换 | 证书有效期 1 年 | `keadm cert rotate --node=<CN> --cert-dir=<目录>`（自动备份旧证书，见 §5.2） |
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

### 灰度发布（分批升级）

`keadm upgrade` 与 `keadm batch --op=upgrade` 均接受两个灰度发布参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--batch-size=N` | `1` | 分批大小：每 N 个节点一批逐批升级 |
| `--pause-between=<duration>` | `0` | 批间暂停时长（如 `30s`、`1m`；`0` 不暂停） |

> 单节点 `keadm upgrade` 接受并校验这两个参数（`batch-size>=1`、`pause-between` 非负且可解析），
> 但单节点升级无分批效果——分批/暂停/fail-fast 在批量模式下生效，
> 支持该参数是为了脚本可对单节点与批量模式统一传参。

```bash
# 灰度发布：每 10 个节点一批，批间暂停 30 秒，任一节点失败即中止
keadm batch --op=upgrade --file=dirs.txt --version=v0.2.0 \
  --batch-size=10 --pause-between=30s
```

灰度语义（与既有 rollback 衔接）：

- **逐批推进**：每批内节点顺序升级（各自备份 + 写台账）；批间按 `--pause-between` 暂停，
  便于观察上一批节点状态后再推进下一批（典型灰度节奏）；
- **fail-fast**：任一节点失败立即中止后续批次，报告成功/失败/未执行三份节点清单；
- **不自动回滚**：失败不影响已成功节点的产物；对失败节点执行
  `keadm rollback --latest --output-dir=<失败节点目录>` 即可恢复（升级失败路径多数在
  备份前就中止、产物未动；已产生备份的场景由 rollback 依据 manifest 校验后事务化恢复），
  各节点备份 id 见升级输出与 `keadm ops-ledger` 台账。

## 5.2 证书轮换（keadm cert rotate）

```bash
# 轮换边缘节点证书（CN 为 edgecore.crt 的 CommonName，约定 edgeflow-<nodeID>）
keadm cert rotate --node=edgeflow-edgecore --cert-dir=./data/certs

# 轮换云端服务端证书（自动继承旧证书 SAN，轮换后仍覆盖边缘访问地址）
keadm cert rotate --node=cloudcore --cert-dir=./data/certs
```

参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--node` | 必填 | 节点证书 CN（`edgecore.crt` 的 CN；云端为 `cloudcore`） |
| `--cert-dir` | `data/certs` | 证书目录（含 `ca.crt`/`ca.key` 与节点证书） |

行为说明：

- **备份先行**：重签前把旧证书/私钥备份到 `<cert-dir>/backups/<时间戳>/`
  （含 manifest.json：CN/时间/文件清单/sha256；同一秒多次轮换自动追加序号防覆盖），
  轮换失败不会破坏旧证书；
- **事务化重签**：复用 `pkg/certs` 的等效 Go 实现（与 `hack/gen-certs.sh` 同一证书布局/
  CN 约定/算法），强制生成新密钥 + 新序列号；先写临时文件并回读校验，全部就绪后
  原子替换旧文件，任一步失败旧证书保持原状；
- **校验报错**：证书目录不存在、节点 CN 不匹配（节点不存在）、CA 缺失（轮换不自动创建
  CA，防止误轮换 CA 导致全量证书失效）均报错退出；
- **重复执行**：每次轮换都是真实轮换（新密钥/新序列号），各生成一个备份目录，
  无临时文件残留；
- **回退**：用备份文件覆盖回原路径即可（`keadm cert rotate` 失败提示中会输出
  `cp` 命令示例）；成功后需将新证书分发到节点并重启 edgecore/cloudcore 生效。

> 与 `hack/gen-certs.sh` 的关系：脚本是 shell 版证书**初始化**工具（幂等跳过，无强制重签
> 能力）；轮换复用其等效 Go 实现 `pkg/certs`（`RotateClientCert`/
> `RotateServerCertWithSANs`），选 Go 方案是为可单测（备份/重签/错误路径全覆盖）。

## 6. 本机验证边界（无集群环境）

本仓库开发环境无 Kubernetes 集群，keadm 的「干净集群安装」验收路径为：

1. **命令级验证**：所有子命令参数校验、退出码、错误提示（`go test ./cmd/keadm`）；
2. **生成物验证**：`cloudcore.yaml` 结构校验（Service NodePort + Deployment
   镜像/探针/卷/TLS env 断言）、`edgecore.env` 键值校验、systemd 单元与安装脚本
   语法检查（`bash -n`）；
3. **真实集群执行**：按上文「在真实集群上执行 / 在真实边缘节点上执行」步骤进行，
   涉及 `kubectl apply`、`systemctl` 的操作需在真实环境完成。
