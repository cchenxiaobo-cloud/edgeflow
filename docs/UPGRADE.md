# keadm 升级与回滚（WBS 10.2）

本文档定稿 EdgeFlow 安装管理 CLI（`keadm`）的**升级/回滚机制**：
`keadm upgrade` / `keadm rollback` / `keadm ops-ledger` 三个子命令
（实现位于 `cmd/keadm/upgrade.go` / `rollback.go` / `opsledger.go`）。

> 边界：本机制作用在 **keadm 生成的产物文件**（离线产物）层面，不操作真实集群
> 与边缘节点，也不触碰数据目录。真实部署（镜像 tag、edgecore 二进制）的升级
> 不在本机制范围内（见 §4 限制条件）。

## 1. 机制说明

### 1.1 版本标记

`keadm join` 生成的 `edgecore.env` 首行注释下含版本标记行：

```
# keadm 产物版本: v0.1.0（keadm join 写入，keadm upgrade 更新，keadm rollback 恢复）
```

- **join 写入**：初始版本 `v0.1.0`（与 `keadm init` 默认镜像 tag `edgeflow/cloudcore:v0.1.0` 对齐）；
- **upgrade 更新**：整行替换为目标版本；
- **rollback 恢复**：从备份快照恢复文件内容；
- 旧版 join 生成的产物无此标记行时，`upgrade` 视当前版本为 `unknown`，并在更新时
  自动追加标记行（兼容升级路径）。

### 1.2 备份模型

`keadm upgrade` 每次执行前先把当前产物备份到 `output-dir/backups/<id>/`：

```
<output-dir>/backups/<id>/
├── manifest.json     # 备份清单：id/ts/version/operator/files/sha256
├── edgecore.env      # 备份时的产物快照
├── edgecore.service
└── install.sh
```

- `<id>` 为到秒的时间戳（如 `20260814-190701`）；同一秒内多次备份追加序号
  （`-2`、`-3`…）避免覆盖；
- `manifest.json` 记录：备份时间（RFC3339）、备份时版本、操作人、
  文件清单、每个文件的 sha256；
- **备份只覆盖三个产物文件**（env/service/install.sh），不碰 README.md、
  数据目录与其他用户文件；
- 备份目录**只增不删**：`reset` 不清理 `backups/`（保留恢复路径与审计现场）；
  备份中途失败留下的无 `manifest.json` 目录会被 `rollback` 视为无效备份跳过
  （保留现场，不自动删除）。

### 1.3 台账模型

所有操作（含失败）追加写入 `output-dir/ops-ledger.jsonl`，**逐行 JSON、追加写、
不覆盖历史**：

```json
{"ts":"2026-08-14T19:17:19+08:00","action":"upgrade","from":"v0.1.0","to":"v0.2.0","result":"failed","operator":"unknown","note":"simulate-failure; 备份=20260814-191719"}
```

| 字段 | 含义 |
| --- | --- |
| `ts` | 操作时间（RFC3339 本地时区） |
| `action` | `upgrade` / `rollback` |
| `from` | 操作前版本（读取不到时为 `unknown`） |
| `to` | 操作目标版本（失败且未知时为空串） |
| `result` | `ok` / `failed` |
| `operator` | 操作人（env `KEADM_OPERATOR`，缺省 `unknown`） |
| `note` | 备注（备份 id、失败原因等） |

失败路径同样落台账（输出目录不存在时自动创建），保证审计完整。

## 2. 操作步骤

### 2.1 升级（keadm upgrade）

```bash
# 升级到 v0.2.0（先备份当前产物 → 更新 env 版本标记 → 台账记录）
keadm upgrade --version=v0.2.0 --output-dir=./keadm-out

# 演练模式：备份完成后模拟失败（用于演练回滚流程，产物不会被修改）
keadm upgrade --version=v0.2.0 --simulate-failure --output-dir=./keadm-out
```

执行流程：

1. **校验版本格式**：必须为 `vX.Y.Z`（如 `v0.2.0`）；格式非法 → 报错退出码 2；
2. **同版本防护**：目标版本与当前版本相同 → 报错退出码 2（不产生无意义备份）；
3. **产物存在性预检**：缺 `edgecore.env` / `edgecore.service` / `install.sh` 任一 →
   报错退出码 1；
4. **备份**：复制三个产物到 `backups/<id>/` 并写 `manifest.json`；
5. **更新版本标记**：替换 `edgecore.env` 的版本标记行；
6. **台账**：追加 `result=ok` 记录。

任一环节失败：**台账记 `failed`**，并提示
「执行 `keadm rollback --latest` 可恢复」。退出码约定：
`0` 成功；`1` 运行时错误（含演练失败）；`2` 参数/用法错误。

### 2.2 回滚（keadm rollback）

```bash
# 取最新有效备份回滚
keadm rollback --latest --output-dir=./keadm-out

# 指定备份 id 回滚（id 见 backups/ 目录名或 ops-ledger 台账）
keadm rollback --backup=20260814-190701 --output-dir=./keadm-out
```

执行流程：

1. **参数校验**：`--backup` 与 `--latest` 互斥，必须二选一（违规退出码 2）；
2. **定位备份**：`--latest` 取 id 最新（时间戳倒序，含序号后缀）；`--backup=<id>`
   精确匹配，不存在则报错退出码 1；
3. **完整性校验**：`manifest.json` 可解析、version/files 字段完整、
   清单中每个文件存在且 sha256 与清单一致；校验失败 → 报错并保留备份目录；
4. **恢复**：逐文件复制回 output-dir，回读校验内容一致，并强制恢复权限
   （env `0600` / service `0644` / install.sh `0755`）；
5. **台账**：追加 `result=ok` 记录（`from`=当前版本，`to`=备份版本）。

### 2.3 台账查询（keadm ops-ledger）

```bash
# 输出最近 20 条（默认）
keadm ops-ledger --output-dir=./keadm-out

# 输出最近 N 条
keadm ops-ledger --limit=5 --output-dir=./keadm-out
```

输出为 JSON 数组（机器可解析，逐行 JSON 原样保留）；台账不存在时输出 `[]`
并以 0 退出；损坏行跳过并附警告。

### 2.4 操作人

所有台账记录的操作人取自环境变量 `KEADM_OPERATOR`，未设置时为 `unknown`：

```bash
KEADM_OPERATOR=alice keadm upgrade --version=v0.2.0 --output-dir=./keadm-out
```

## 3. 异常路径表

| # | 异常场景 | 行为 | 兜底/恢复 |
| --- | --- | --- | --- |
| 1 | **升级中途失败**（备份失败 / 更新版本标记失败 / 写台账失败） | 报错退出码 1，台账记 `failed` | 提示执行 `keadm rollback --latest`；已完成备份保留可用 |
| 2 | **回滚本身失败**（备份缺失 / 完整性校验失败 / 恢复复制失败） | 报错退出码 1，台账记 `failed`，**备份目录保留不删** | 输出人工介入路径：逐文件 `cp backups/<id>/<file> <output-dir>/<file>` 命令示例 |
| 3 | **数据迁移异常**（升级/回滚涉及的数据文件异常） | 本机制**不接触数据目录**：备份/恢复只处理三个产物文件，SQLite 数据（如 `/var/lib/edgeflow/edgeflow.db`）不在操作范围内 | 数据一致性由升级前自行快照校验；演练建议：升级前对数据目录做 hash 基线，操作后比对（见 §5 演练） |
| 4 | 版本格式非法 / 同版本升级 / 参数互斥 | 退出码 2，台账记 `failed`（非法参数不产生备份） | 修正参数重试 |
| 5 | 无可用备份时 rollback | 退出码 1，台账记 `failed` | 无自动恢复路径；需重新 `keadm join` 生成产物 |

## 4. 限制条件

- **仅产物文件级回滚**：备份/恢复只覆盖 `edgecore.env`、`edgecore.service`、
  `install.sh` 三个产物文件；`README.md`、`keadm-manifest.json`（reset 校验清单）、
  用户自建文件一律不动；
- **数据目录不动**：边缘节点上的 SQLite 元数据（`/var/lib/edgeflow/edgeflow.db`）
  与云端数据不在本机制操作范围内——升级/回滚前后数据一致性需另行保障
  （升级前快照、升级后校验）；
- **真实部署升级未覆盖**：镜像 tag 变更、edgecore 二进制替换、节点侧
  `install.sh` 的实际执行均不在 keadm 产物级机制内；本机制保证的是
  「离线产物」可恢复，真实节点回滚需结合发布流程（v0.1.0 已发布，见
  docs/DEPLOYMENT.md §5.2 与 docs/REAL-CLUSTER-GUIDE.md）；
- **人工介入入口**：任何自动恢复不可用的场景，备份目录保留完整快照，
  可直接用 `cp` 命令手动恢复（错误提示中给出逐文件命令示例）；
- **reset 交互**：`keadm reset` 按校验清单删除产物；`upgrade` 更新 env 后其
  哈希与 `keadm-manifest.json` 不一致，`reset` 会跳过该文件并提示人工确认
  （安全设计，防误删）；`rollback` 恢复原内容后哈希自动一致；
- **幂等性**：upgrade 同版本拒绝（退出码 2）；重复升级到更高版本每次都会
  产生新备份（备份只增不删，长期使用注意磁盘占用，建议定期人工归档）。

## 5. 端到端演练（2026-08-14 实测）

演练目录 `/tmp/e2e-upgrade-drill`（命令输出见交付记录）：

```bash
keadm init --output-dir=$OUT
keadm join --cloudcore-ip=192.168.1.10 --token=abc123 --node-id=edge-worker-01 --output-dir=$OUT
sha256sum $OUT/edgecore.env $OUT/edgecore.service $OUT/install.sh > baseline.sha   # 产物基线
sha256sum data/edgeflow.db out/user-notes.md > data-baseline.sha                    # 数据基线
keadm upgrade --version=v0.2.0 --simulate-failure --output-dir=$OUT                 # 预期失败 exit=1
keadm rollback --latest --output-dir=$OUT                                           # 恢复，diff 一致
keadm upgrade --version=v0.2.0 --output-dir=$OUT                                    # 真实升级
keadm rollback --latest --output-dir=$OUT                                           # 真实回滚
sha256sum -c baseline.sha data-baseline.sha                                         # 产物/数据均一致
keadm ops-ledger --output-dir=$OUT                                                  # 台账 4 条记录
```

实测结论：

- 演练失败路径：升级失败 exit=1、台账 `failed`、产物 hash 与基线一致（未修改）；
- 回滚路径：版本恢复 `v0.1.0`、三个产物与备份 `diff` 无差异、权限恢复
  （install.sh 可执行）；
- 数据路径：模拟 SQLite 数据文件与 output-dir 内用户文件全程 hash 一致（未触碰）；
- 台账：`upgrade failed` / `rollback ok` / `upgrade ok` / `rollback ok` 四条记录，
  操作人默认 `unknown`，`KEADM_OPERATOR=alice` 时记录 `alice`；
- 单测：`go test -race ./cmd/keadm/` 全绿（含既有 init/join/reset/version 用例，
  覆盖率 74.6%）。

## 6. 与文档/实现一致性自查

| 本文档声明 | 实现位置 | 一致 |
| --- | --- | --- |
| 版本标记行格式 | `upgrade.go` `envVersionPrefix/Suffix`、`join.go` 模板 `VersionLine` | ✅ |
| 备份目录结构与 manifest 字段 | `upgrade.go` `createBackup` / `backupManifest` | ✅ |
| 台账字段与追加写 | `upgrade.go` `appendLedger`（O_APPEND） | ✅ |
| upgrade 四步流程与失败兜底 | `upgrade.go` `runUpgrade` | ✅ |
| rollback 定位/校验/恢复/兜底 | `rollback.go` `runRollback` / `findBackup` / `restoreBackup` | ✅ |
| ops-ledger 最近 N 条（默认 20） | `opsledger.go` `runOpsLedger` | ✅ |
| 退出码约定 0/1/2 | `main.go` `exitOK/exitRuntime/exitUsage` | ✅ |
| 操作人 env `KEADM_OPERATOR` | `upgrade.go` `operatorName` | ✅ |
| 异常路径表三类兜底 | §3 与 `runUpgrade`/`runRollback` 各失败分支 | ✅ |
| 演练记录与数据不动 | §5，2026-08-14 实测 | ✅ |
