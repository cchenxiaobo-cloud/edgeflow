# CODE-REVIEW-M4D — EdgeFlow 第四轮复核（M4D 复核员）

- 复核日期：2026-08-14
- 复核范围：
  1. keadm 升级回滚机制（WBS 10.2）— 7aa035c + eabb360（协调点修复）
  2. M4C P2×6 处置 — a1c619d + 4f8eeab
  3. M5 前置文档与示例（9.2-9.5）— f090a30
- 复核方式：代码走读 + 测试断言核对 + 本地命令验证（build/vet/test -race/cover/lint）+ 运行时 e2e 实测 + demo.sh 实测
- 约束：只读复核，未修改任何代码

## 审查状态总览

| 维度 | 状态 | 结论摘要 |
|---|---|---|
| 1. 升级回滚机制 | ✅ 完成 | 机制完整、文档一致；3 项 P2 建议 |
| 2. P2 处置六项 | ✅ 完成 | ①②③⑤ 修复真实、④⑥ 台账合理；1 项 P2（测试缺口） |
| 3. M5 文档一致性 | ✅ 完成 | 与实现一致、demo 实测 PASS；1 项 P2（文档时序缺口） |
| 4. 生产就绪度 | ✅ 完成 | 异常路径齐全；1 项 P2（台账/退出码不一致） |
| 5. 命令验证 | ✅ 完成 | build/vet/race 24 包全绿、lint 0、gofmt 干净、e2e 实测通过 |

---

## 1. 升级回滚机制（WBS 10.2）

### 1.1 备份/恢复模型（upgrade.go / rollback.go）

**✅ 符合设计且有实测支撑：**

- 备份 `backups/<ts>/`：manifest.json（id/ts/version/operator/files/sha256）+ 3 产物；同秒多次备份追加序号（`-2`/`-3`）不覆盖；
- 中途失败留下的无 manifest 目录：`listBackups` 跳过并警告，**保留现场不删**（文档 §1.2 一致）；
- manifest 最后写入（先文件后清单），失败时不会出现「有清单无文件」的假完整备份；
- rollback 完整性校验：manifest 可解析 + version/files 非空 + 逐文件存在且 sha256 一致，任一不符拒绝恢复；
- 恢复：逐文件复制 + **回读校验逐字节一致** + 强制权限（env 0600 / service 0644 / install.sh 0755）；失败保留备份并输出逐文件 `cp` 人工介入命令；
- **部分恢复失败场景**：restoreBackup 逐文件执行，若中途失败会留下「部分已恢复」的混合状态（例如 env 已恢复、install.sh 未恢复）。兜底充分：备份保留 + 人工 cp 提示 + 重跑 rollback 幂等可完成剩余恢复。**建议**：文档 §3 异常表第 2 行补充「部分恢复」明示（P2-4，见 §6）。

### 1.2 台账（opsledger.go / appendLedger）

**✅ 追加写原子性**：`os.OpenFile(O_APPEND|O_CREATE)` + 单次 `Write(JSON+\n)`，POSIX 下 O_APPEND 单次小写入对并发追加原子，多 keadm 并行不会互相交错损坏行；`ops-ledger` 读取时跳过损坏行并警告（容忍外部破坏）。
- 失败路径同样落台账（含 output-dir 不存在时自动创建目录）——审计完整性好。
- 并发升级无锁（env 最后写入者胜）：CLI 场景可接受，文档未承诺并发，不设阻塞。

### 1.3 --simulate-failure 语义

**✅ 与文档一致**：备份完成后模拟失败（env 未更新、产物未修改），退出码 1，台账 `failed` 且 note 含 `simulate-failure; 备份=<id>`，提示可 `rollback --latest`。备份保留用于演练回滚（回滚后内容与现状一致，幂等无害）。测试 `TestUpgradeSimulateFailure` 断言了退出码/台账/产物不变三要素。

### 1.4 版本标记与旧产物兼容

- 格式校验 `^v[0-9]+\.[0-9]+\.[0-9]+$` 严格；`TestUpgradeInvalidVersion` 覆盖 8 种非法形态（含 `v0.2.0-rc1`、`V0.2.0`）；
- 同版本拒绝（exit 2，不产生无意义备份）；
- 旧 join 产物无标记 → `envVersion` 返回 `unknown` → upgrade 自动在文件末尾**追加**标记行（兼容升级路径），文档 §1.1 一致；
- `envVersion` 行尾后缀（`（...）`）剥离逻辑正确（取 `（` 前版本号）。

### 1.5 reset 与 upgrade/rollback 交互（协调点 eabb360）

**✅ 合并式刷新正确，运行时实测通过**：

- `refreshManifest` 先 `loadManifest` 再合并更新，不整体重建——init 的 cloudcore.yaml/NOTES.txt 校验记录在 upgrade/rollback 后保留（实测：init+join 共 6 项，upgrade 后 6 项全在、env 哈希已更新）；
- rollback 后 env 哈希与清单一致（实测 `6e7739a4…` 前后匹配）→ reset 可全删（实测「已删除 5 个」）；
- 篡改 env 后 reset 跳过该文件并提示人工确认（实测 4 删 1 跳）；
- reset 不删 `backups/` 与 `ops-ledger.jsonl`（实测保留）——恢复路径与审计现场完整；
- **发现**：upgrade 的 `refreshManifest` 失败分支台账记 `result:"ok"` 但进程退出码 1（代码注释语义为「升级本身已完成」）——**台账与退出码不一致**，脚本按退出码判定会与台账矛盾（P2-1，见 §6）。

---

## 2. M4C P2×6 处置

### ① TLSSAN fail-fast（cmd/cloudcore/main.go + parseSANList）

**✅ 修复真实**：`parseSANList` 对空条目/未知前缀/非法 IP/空 DNS 全部返回错误；`run` 在 TLS=on 且 SAN 非法时 `log.Errorf + return 1`（启动即失败，不再 Warn 跳过）。
- **合法用例不受影响**：仅 `EDGEFLOW_CLOUDCORE_TLS=on` 时进入校验分支；合法 SAN（IP+DNS 混合、逗号后空格）正常解析。
- 测试真实：`TestParseSANList`（5 场景）+ `TestRunInvalidTLSSAN`（坏条目 → exit 1）。

### ② 校验和清单四路径刷新（manifest.go + reset.go）

**✅ 四条路径全部刷新**：

| 路径 | 实现 | 验证 |
|---|---|---|
| init | `recordGeneratedFiles(initOutputs)` + saveManifest（init.go L96-105） | 代码确认 |
| join | 同上 joinOutputs | 代码确认 |
| upgrade | `refreshManifest(backupFiles)`（env 更新后） | 实测：6 项保留、env 哈希更新 |
| rollback | `refreshManifest(backupFiles)`（恢复后） | 实测：哈希与清单一致 → reset 全删 |

- reset 语义：有清单→哈希匹配才删、不匹配跳过提示；无清单（旧产物）→保持白名单行为+警告；清单损坏→**保守不删**。防误删设计闭环。

### ③ unit ID + 连接上限（pkg/modbussim）

**✅ 修复真实**：
- unit ID 仅接受 1-247；0（广播）与 248-255（保留）按规范应答异常码 0x0B（网关目标无响应），不回显成功；测试 `TestUnitIDOutOfRangeRejected` 断言 fc=0x83 + exc=0x0B + unit 回显；
- `maxConns` 默认 8、`WithMaxConns` 可调；超限新连接直接关闭（读侧 EOF），存量连接不受影响；测试 `TestMaxConnsRejectsExcess`（上限 2、第 3 连被关、存量正常收发）真实。
- 说明：unit 0 广播按严格规范从站不应答，此处回 0x0B 异常——对单设备模拟器属合理简化（显式错误优于静默），不设阻塞。

### ④ 关闭（NodeController 陈旧快照窗口）

**✅ 台账合理**：NODECONTROLLER.md §6 文档化为已知边界（check-then-act 概率极低、下一心跳自愈），关闭无需改码。结论与证据链完整。

### ⑤ 0.1°C 舍入（mappers/modbus）

**✅ 修复真实**：`math.Round(value*scaleFactor)` 四舍五入而非截断；值域 [0,100] 前置校验，舍入后原始值 ∈ [0,1000] 不越界（99.99→1000 边界安全）。测试 `TestHandleCommandTargetTempRounds` 断言 25.55→25.6（截断实现会是 25.5）、99.99→100.0，断言真实且能区分新旧实现。

### ⑥ 延后（:latest immutable/cosign/SBOM）

**✅ 台账合理**：依赖镜像仓库 + cosign 基础设施，延后至 M5 发布阶段；风险登记 MULTIARCH.md §5 风险表 + PROGRESS.md §5 待办（含 P2 条目），跟踪闭环。

---

## 3. M5 前置文档与示例一致性（9.2-9.5）

### 3.1 API-SPEC.md（9.2）抽查

- 端点总览 13 项与实现路由核对一致（/healthz、nodes、edgenodes、pods、devices、podsync、config-sync、device-command）；
- 错误码表（400/404/500/502/504 + 语义区分指引）与 `sendDeviceCommand`/`sendPodSync` 等实现的分支一致；
- device-command：请求体字段（deviceName/namespace/property/value）与 `deviceCommandRequest` 完全对应；响应 `{"status":"ok","acked":true}` 与实现一致；
- 设备状态 JSON 形态（properties/desired/lastReportedAt）与 `devicestatus.DeviceStatus` json tag 逐一对应。

### 3.2 examples/demo.sh（9.5）——实测验证

**✅ 本次复核实测：DEMO PASS（2026-08-14 19:25，含 MQTT 数据面全段）**，与 REVIEWS.md 记录的第 1/2 次构成第 3 次 PASS（任务描述「3 次 DEMO PASS」成立）。

- 断言形态核实：`"targetTemp":25` 匹配 devices API 的 `"desired":{"targetTemp":25}`（Go json.Encoder 紧凑输出无空格，grep 可靠）；`"status":"Ready"` 匹配节点注册表输出；
- 幂等性：RUN_SUFFIX（时间戳+PID）随机 node-id/Pod 名 + 随机端口（python3 优先、lsof 回退、三端口互异循环）——重复运行互不冲突；
- 清理完整性（实测 0 残留）：podsync delete 回收容器 + trap EXIT 兜底（精确容器名 + 仅删 mktemp 路径）+ stop_proc 优雅停止（SIGTERM→10s→SIGKILL）；0 容器 / 0 进程 / 0 临时目录；
- `bash -n` 语法通过；`set -euo pipefail` + die() 失败即退出并输出日志路径。

### 3.3 REVIEWS.md 评审记录真实性

- 覆盖度核对表与代码逐项相符（我抽查 5 处全部一致）；两次 DEMO PASS 记录 + 我的第 3 次实测互相印证；残留检查记录与我的复检一致；
- P2 遗留项（demo.sh grep 断言、镜像拉取、mosquitto 客户端依赖）为事实陈述，合理。

### 3.4 ⚠️ 文档时序缺口（P2-2，见 §6）

f090a30（文档定稿）先于 7aa035c（keadm upgrade/rollback 实现合入），导致：

- **HANDOFF.md L43**：「keadm/ # 安装管理 CLI：init/join/reset/version（+ M5: upgrade/rollback）」——upgrade/rollback 已实现，表述过时；
- **HANDOFF.md L222** 与 **DEPLOYMENT.md L250/L386**：「keadm upgrade/rollback 由 M5 并行任务实现」——REVIEWS.md 自己标注了「合入后需复核 §5 引用有效性」，但本轮提交未包含该复核更新；
- **HANDOFF.md §8 命令速查缺 `keadm upgrade/rollback/ops-ledger` 条目**（实测命令可用）。

---

## 4. 生产就绪度

### ✅ 强项

- 异常路径：upgrade 5 类失败、rollback 4 类失败全部落台账并给出恢复指引；三类兜底（提示 rollback / 人工 cp / 无备份明示）与文档 §3 异常表一致；
- 退出码约定 0/1/2 贯穿所有子命令，测试逐项断言；
- 日志输出规范：stdout 仅正常结果，stderr 承载错误与提示（flag 输出统一走 stderr）；
- 文档一致性自查表（UPGRADE.md §6）逐项有实现位置，且与我的走读结论一致；§5 声称 keadm 覆盖率 74.6%，与实测 74.6% 吻合。

### ⚠️ 测试缺口（P2-3，见 §6）

- **eabb360 协调修复零直接单测**：`refreshManifest` 合并语义仅有 `TestManifestMergesInitAndJoin` 间接覆盖（且不经过 upgrade/rollback）；`defaultNodeID`/`sanitizeNodeIDChars`（macOS 点号主机名场景）**无任何测试**；
- 「升级→reset 全删」「回滚→reset 全删」链路无自动化回归测试（本次复核实测通过，但无测试保护）。

### 已知缺口（设计内，不阻塞）

- 备份只增不删（文档 §4 明示磁盘占用，建议人工归档）；
- 真实部署升级（镜像 tag/二进制/节点 install.sh 执行）不在产物级机制内（文档 §4 明示，M5 发布流程闭环）；
- 数据目录（SQLite）不在备份范围（文档 §4 + §5 演练给出 hash 基线法）。

---

## 5. 命令验证结果（2026-08-14 复核实测）

| 命令 | 结果 |
|---|---|
| `go build ./...` | ✅ 通过 |
| `go vet ./cmd/keadm/... ./pkg/modbussim/... ./mappers/modbus/... ./cmd/cloudcore/...` | ✅ 0 问题 |
| `go test -race -cover ./cmd/keadm/...` | ✅ 通过，覆盖 74.6% |
| `go test -race -cover ./pkg/modbussim/...` | ✅ 通过，覆盖 88.1% |
| `go test -race -cover ./mappers/modbus/...` | ✅ 通过，覆盖 71.7% |
| `go test -race -cover ./cmd/cloudcore/...` | ✅ 通过，覆盖 82.5% |
| `go test -race -cover ./...` | ✅ 24 包全绿（覆盖 71.7%-100%） |
| `golangci-lint run ./...` | ✅ 0 issues |
| `gofmt -l`（4 个目标包 + examples） | ✅ 无差异 |
| 运行时 e2e：init→join→upgrade v0.2.0→rollback→reset | ✅ 实测通过（详见 §1.5） |
| `bash -n examples/demo.sh` + 完整运行 | ✅ DEMO PASS（含 MQTT 段），0 残留 |

---

## 6. 最终结论

## ✅ 有条件通过（0 P0 / 0 P1 / 5 P2）

三块交付质量高：升级回滚机制设计完整、异常路径闭环、文档与实现逐项自洽；P2×6 处置真实（修复均有可区分新旧实现的测试断言）；demo.sh 三次实测 PASS、无残留；全量 24 包 race 全绿、lint 0。

### P2 清单（不阻塞交付，建议下轮闭环）

| # | 级别 | 问题 | 建议 |
|---|---|---|---|
| P2-1 | 台账/退出码一致性 | upgrade 的 refreshManifest 失败分支：台账 `result=ok` 但退出码 1 | 台账记 `failed`（note 注明「升级已完成，仅清单刷新失败」），或增加 `partial` 状态 |
| P2-2 | 文档时序缺口 | HANDOFF.md L43/L222、DEPLOYMENT.md L250/L386 仍写「M5 并行任务实现」；HANDOFF §8 命令速查缺 upgrade/rollback/ops-ledger | 更新为已实现（WBS 10.2 已合入），补命令速查条目，闭环 REVIEWS.md 的「合入后需复核」标注 |
| P2-3 | 测试缺口 | eabb360 协调修复（defaultNodeID 清洗、refreshManifest 合并）零直接单测；升级→reset 链路无回归测试 | 补 `TestDefaultNodeIDSanitize`（含点号/连续非法字符/全非法→edge-local）、`TestRefreshManifestPreservesInitRecords`、升级→reset 全删用例 |
| P2-4 | 部分恢复 | restoreBackup 非事务性：中途失败留混合状态（有兜底：备份保留+cp 提示+重跑幂等） | 文档 §3 异常表补充「部分恢复」明示；可选：恢复前先全部复制到临时文件再原子替换 |
| P2-5 | 纵深防御 | findBackup/restoreBackup 未校验 manifest 文件清单 ∈ {3 个产物名}（被篡改清单可写出 outputDir，本地威胁模型下低危） | 恢复前校验 name 仅含 `[a-zA-Z0-9._-]` 且 ∈ backupFiles |

### 复核签名

- 复核人：M4D 复核员（只读复核，未修改任何代码/产物）
- 复核时间：2026-08-14 19:22-19:40 (Asia/Shanghai)
- 报告路径：docs/CODE-REVIEW-M4D.md（本文件，完整写入）
