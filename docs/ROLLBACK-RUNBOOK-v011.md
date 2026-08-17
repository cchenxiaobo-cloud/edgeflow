# EdgeFlow v0.1.1 回滚手册（Rollback Runbook）

> 状态：✅ 已入档（2026-08-18；发布准备员起草，报告核验员核验口径一致，主线修正 3 处：checksums 12 条目、36 包口径、灰度走 keadm batch）｜演练脚本为草案，实战前需人工审核。
> 适用范围：v0.1.1 → v0.1.0 回滚（v0.1.1 发布后首次回滚场景）。
> 原则：**先恢复可用，再排查根因**（与 docs/DRILL-SCHEDULE.md §5 一致）。
> 依据：docs/UPGRADE.md（keadm upgrade/rollback 机制）、docs/MULTIARCH.md §6（镜像回退）、
> docs/DEPLOYMENT.md §5（升级回滚注意）、docs/SECURITY.md §5（证书吊销与 CRL 路径）、
> docs/ARCHITECTURE.md §7.2（热重载边界）、docs/KEADM.md（keadm 子命令全集）。

---

## 1. 触发条件（回滚决策）

> 满足**任一条件**即触发回滚评估；**同时满足多条** → 立即执行回滚，不等人工确认。

| # | 条件 | 判定标准 | 参照 |
|---|------|---------|------|
| A | **健康检查连续失败** | cloudcore `/healthz` 连续 3 次非 200（liveness 探针：10s 起检 / 间隔 10s；DEPLOYMENT.md §2.4） | prometheus `up{job="edgeflow-cloudcore"}==0` for 1m |
| B | **/metrics 异常** | `/metrics` 端点无响应/scrape 失败/指标缺失超过 5 分钟 | monitoring-alerting-v011.md §3 CloudCoreDown |
| C | **注册成功率 < 99%** | 灰度/全量后注册成功率跌破 99%（基线 100%，PERFORMANCE-BASELINE.md） | 通过 load-test 或 API 审计计量 |
| D | **告警风暴** | 5 分钟内 ≥3 条 critical 告警同时激活 | monitoring-alerting-v011.md §4 告警风暴熔断 |
| E | **安全事件** | mTLS 握手批量失败（证书异常/CRL 误杀）/ OCSP 服务异常 | 研发安全负责人判定 |

**回滚评估流程**（条件 A/B/C/D/E 触发后）：

1. 值班确认告警真实性（非 transient false positive）；
2. 检查 `ops-ledger` + `audit-ledger` 确认最近变更（升级记录）；
3. 若确认与 v0.1.1 升级相关 → 立即执行 §3 回滚；
4. 若疑似非变更引起 → 排障优先，但回滚仍作为兜底选项（收益 > 风险时果断执行）。

---

## 2. 回滚前准备（≤5 分钟，回滚前必做）

```bash
# 1. 记录当前状态（回滚后对比用）
cd /Users/mac/Documents/edgeflow
curl -s http://127.0.0.1:8080/healthz | tee /tmp/rollback-pre-healthz.json
curl -s http://127.0.0.1:8080/api/v1/nodes | jq -r '.[].nodeID' | tee /tmp/rollback-pre-nodes.txt
curl -s http://127.0.0.1:8080/api/v1/pods | jq -r '.[].phase' | sort | uniq -c | tee /tmp/rollback-pre-pods.txt

# 2. 确认 v0.1.0 制品可用（release/v0.1.0/ 归档）
ls -la release/v0.1.0/ | grep -E 'cloudcore|edgecore|keadm|edgeflow-0.1.0.tgz'
shasum -a 256 -c release/v0.1.0/checksums.txt   # 全部 OK 方可继续

# 3. 备份当前数据（SQLite + 台账）
cp data/edgeflow.db data/edgeflow.db.rollback-pre-$(date +%s)   # 边缘侧
cp data/edgeflow.db /tmp/edgeflow-cloud.db.rollback-pre-$(date +%s)  # 如有云端 db
# audit-ledger 为追加写，不做覆盖式备份——回滚后补记事件即可
```

---

## 3. 回滚步骤（分场景）

### 3.1 ① 二进制回滚（keadm 产物，边缘节点侧）

**适用**：edgecore 二进制 / keadm 产物（env/service/install.sh）通过 keadm upgrade 部署。

```bash
# 标准路径：回滚到最近一次升级前的备份
keadm rollback --latest --output-dir=./keadm-out

# 指定备份 ID（backups/ 目录名，如 20260814-190701）
keadm rollback --backup=20260814-190701 --output-dir=./keadm-out

# 验证回滚结果
diff keadm-out/edgecore.env keadm-out/backups/<id>/edgecore.env   # 无差异 = 成功
```

**回滚失败兜底**（UPGRADE.md §3 异常路径表）：

```bash
# 若 keadm rollback 失败（备份缺失/校验失败），手动逐文件恢复：
BACKUP_ID=$(ls -t keadm-out/backups/ | head -1)
cp "keadm-out/backups/$BACKUP_ID/edgecore.env"    keadm-out/edgecore.env
cp "keadm-out/backups/$BACKUP_ID/edgecore.service" keadm-out/edgecore.service
cp "keadm-out/backups/$BACKUP_ID/install.sh"       keadm-out/install.sh
# 权限恢复（0600 env / 0644 service / 0755 install.sh — UPGRADE.md restoreBackup 事务化口径）
chmod 0600 keadm-out/edgecore.env
chmod 0644 keadm-out/edgecore.service
chmod 0755 keadm-out/install.sh
```

**注意**：回滚**不恢复** ops-ledger.jsonl（台账为追加写审计记录，回滚事件另行追加），
台账文件保留完整操作历史。

### 3.2 ② 镜像回滚（cloudcore / edgecore）

**适用**：cloudcore 经 Helm 部署（Kubernetes）、edgecore 经容器运行。

**cloudcore（Helm）**：

```bash
# 回滚到 v0.1.0 Chart 版本
helm rollback edgeflow <REVISION>   # REVISION 以 helm history 输出为准，v0.1.0 对应修订号

# 或显式指定旧镜像（用 images.json v0.1.0 digest）
helm upgrade edgeflow build/charts/edgeflow \
  --set cloudcore.image.repository=<reg>/edgeflow/cloudcore \
  --set cloudcore.image.tag=v0.1.0

# 验证
kubectl get pods -l app.kubernetes.io/instance=edgeflow
kubectl port-forward svc/edgeflow-cloudcore 8080:8080 && curl http://127.0.0.1:8080/healthz
```

**edgecore（容器）**：

```bash
# 停止 v0.1.1 容器，启动 v0.1.0 镜像（用 digest 不可变引用）
docker stop edgecore
docker rm edgecore
docker run -d --name edgecore \
  -e EDGEFLOW_EDGECORE_CLOUD_ADDR=ws://<cloud-ip>:<port>/v1/edge \
  -e EDGEFLOW_EDGECORE_NODE_ID=<node-id> \
  -v /var/lib/edgeflow:/data \
  <reg>/edgeflow/edgecore@sha256:<旧 digest>   # 旧 digest 见 release/v0.1.0/images.json
```

**镜像回退到旧 digest（双架构，MULTIARCH.md §6）**：

```bash
# 把 v0.1.1 tag 重新指回 v0.1.0 的 digest（不推荐，仅应急）
# 优先用 v0.1.0 tag 或 digest 直接引用；以下为紧急覆写操作：
docker buildx imagetools create -t <reg>/edgeflow/cloudcore:v0.1.1 \
  <reg>/edgeflow/cloudcore@sha256:<旧 digest>   # 旧 digest 见 MULTIARCH.md §6
```

### 3.3 ③ 配置回滚

**适用**：升级后配置文件（config/cloudcore.json、config/edgecore.json）变更导致异常。

```bash
# 恢复备份配置文件
cp config/cloudcore.json.bak config/cloudcore.json
cp config/edgecore.json.bak   config/edgecore.json

# 发送 SIGHUP 热重载（ARCHITECTURE.md §7.2）
pkill -SIGHUP -f 'bin/cloudcore'
pkill -SIGHUP -f 'bin/edgecore'
```

**热重载边界（必须知道）**：

| 配置项 | 热生效 | 需重启 | 说明 |
|--------|--------|--------|------|
| cloudcore `port` | ✅ SIGHUP 热切换 | — | 绑定失败回滚旧监听（ARCHITECTURE.md §7.2） |
| edgecore `podReportInterval` / `deviceReportInterval` | ✅ 下一周期生效 | — | ticker 重置 |
| cloudcore `hubPort` / `compress` | ❌ | ✅ 重启 | 连接参数在注册时确定 |
| edgecore `cloudAddr` / `nodeID` / `reconcileInterval` | ❌ | ✅ 重启 | 连接/身份/调谐在装配期固定 |

**配置回滚后**：通过 `/metrics` 快照（`edgeflow_cloudcore_*` 指标）确认运行中配置与预期一致。

### 3.4 ④ 数据回滚

> ⚠️ 数据回滚为最后手段（比配置回滚风险高）；仅在**数据损坏/误操作**时执行。
> 原则：回滚前先备份当前状态（即使已损坏，保留现场用于根因分析）。

**SQLite 元数据库（edgecore，`data/edgeflow.db`）**：

```bash
# 停止 edgecore 后恢复
kill -TERM <edgecore_pid>   # 优雅退出（flush WAL）

# 恢复备份（WAL 模式：先确认 wal 文件已合并）
# 备份文件来自 §2 步骤 3 或日常备份策略
cp data/edgeflow.db.rollback-pre-<ts> data/edgeflow.db
rm -f data/edgeflow.db-wal data/edgeflow.db-shm    # 清除旧 WAL，让 SQLite 重建

# 重启 edgecore
./bin/edgecore
```

**op-ledger（Modbus 操作台账，SQLite 内表）**：随 SQLite 恢复一并恢复（30 天保留，KEADM.md）。

**audit-ledger（云端审计台账，`audit-ledger.jsonl`）**：

- **不回滚**：audit-ledger 为追加写 JSONL（ARCHITECTURE.md §6.5），设计为不可变审计链；
- 回滚操作发生后，**追加**一条 rollback 事件记录（不在 audit-ledger 中，在 ops-ledger 中）：
  ```bash
  echo '{"ts":"<ISO8601>","op":"rollback","from":"v0.1.1","to":"v0.1.0","operator":"<值班人>","trigger":"<条件编号>"}' >> keadm-out/ops-ledger.jsonl
  ```

**ops-ledger（keadm 运维台账，`ops-ledger.jsonl`）**：追加写，不覆盖（UPGRADE.md §2.3）。
回滚成功后追加一条记录，不要覆盖已有台账。

---

## 4. 回滚验证（回滚后必做，≤5 分钟）

| # | 验证项 | 命令 | 预期 |
|---|--------|------|------|
| 1 | healthz | `curl -s http://127.0.0.1:8080/healthz` | `{"status":"ok","version":{...}}` |
| 2 | 节点注册恢复 | `curl -s http://127.0.0.1:8080/api/v1/nodes \| jq '[.[].status]'` | 全部 `Ready`（NodeController 180s 内） |
| 3 | 台账查询 | `keadm ops-ledger --limit=5 --output-dir=./keadm-out` | 最新一条为 rollback 事件 |
| 4 | 版本确认 | `./bin/cloudcore --version` / `./bin/edgecore --version` | `version=v0.1.0` |
| 5 | 业务冒烟 | `curl -X POST .../api/v1/nodes/<nodeID>/podsync -d '{"operation":"add","pod":...}'` | `{"status":"ok","acked":true}` |
| 6 | 指标恢复 | `curl -s http://127.0.0.1:8080/metrics` | 五指标均正常输出，active_connections > 0 |

**判定**：6 项全部通过 → 回滚成功，通知值班/审批人，更新台账。
任一项不通过 → 回滚失败，按 §6 升级处置。

---

## 5. 回滚演练脚本草案（bash）

> 用途：在测试/预发环境模拟 v0.1.1 升级→故障→回滚→验证全链路。
> 参照 UPGRADE.md §5 端到端演练（离线已 6/6 通过），本脚本为 v0.1.1 定制版。
> ⚠️ 草案，执行前需人工审核（确认路径/版本/节点 ID 正确）。

```bash
#!/bin/bash
# hack/rollback-drill-v011.sh — v0.1.1 回滚演练脚本（草案）
# 用法：bash hack/rollback-drill-v011.sh <node-id> <output-dir> <cloud-addr>
# 示例：bash hack/rollback-drill-v011.sh edge-node-1 ./keadm-out ws://127.0.0.1:10000
set -euo pipefail

NODE_ID="${1:?需要 NODE_ID}"
OUT="${2:?需要 output-dir}"
CLOUD_ADDR="${3:-ws://127.0.0.1:10000/v1/edge}"
DRY_RUN="${DRY_RUN:-0}"   # DRY_RUN=1 仅打印不执行

log() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; exit 1; }

# ---- 1. 基线快照 ----
log "1/9 基线快照..."
[ "$DRY_RUN" = 1 ] && { log "DRY_RUN"; exit 0; }
sha256sum "$OUT"/edgecore.env "$OUT"/edgecore.service "$OUT"/install.sh > /tmp/rollback-drill-baseline.sha256
cp data/edgeflow.db data/edgeflow.db.drill-bak
log "  基线 sha256 已保存到 /tmp/rollback-drill-baseline.sha256"

# ---- 2. 升级（模拟失败） ----
log "2/9 升级模拟失败..."
keadm upgrade --version=v0.1.1 --simulate-failure --output-dir="$OUT" && fail "应失败但成功" || log "  预期失败 exit=1 ✅"

# ---- 3. 台账确认失败记录 ----
log "3/9 台账确认..."
keadm ops-ledger --limit=1 --output-dir="$OUT" | grep -q '"result":"failed"' || fail "台账无失败记录"
log "  台账 failed 记录 ✅"

# ---- 4. 回滚（产物） ----
log "4/9 keadm rollback --latest..."
keadm rollback --latest --output-dir="$OUT"
log "  回滚完成"

# ---- 5. 产物一致性 ----
log "5/9 产物与基线 diff..."
sha256sum -c /tmp/rollback-drill-baseline.sha256 || fail "回滚后产物与基线不一致"
log "  diff 一致 ✅"

# ---- 6. 台账 rollback 记录 ----
log "6/9 台账 rollback 记录..."
keadm ops-ledger --limit=1 --output-dir="$OUT" | grep -q '"op":"rollback"' || fail "台账无 rollback 记录"
log "  台账 rollback ✅"

# ---- 7. 升级（真实） ----
log "7/9 真实升级到 v0.1.1..."
keadm upgrade --version=v0.1.1 --output-dir="$OUT"
log "  升级完成"

# ---- 8. 回滚（真实） ----
log "8/9 真实回滚到 v0.1.0..."
keadm rollback --latest --output-dir="$OUT"
sha256sum -c /tmp/rollback-drill-baseline.sha256 || fail "回滚后产物不一致"
log "  回滚 + 校验 ✅"

# ---- 9. 清理 ----
log "9/9 清理 + 恢复正式版本..."
rm /tmp/rollback-drill-baseline.sha256
log "  DRILL PASS ✅"
```

---

## 6. 回滚失败升级处置

| 失败场景 | 升级动作 |
|---------|---------|
| 回滚后 healthz 仍非 200 | 检查 cloudcore 进程/端口绑定；必要时 `helm rollback` 回退整个部署 |
| 节点注册失败 | 检查 mTLS 证书（回滚后证书目录是否匹配旧版本）、CloudHub 端口；重启 edgecore 触重重连 |
| 数据恢复异常 | 保留当前损坏文件（现场）+ 使用更早备份（data/edgeflow.db.drill-bak 等） |
| 回滚本身失败（keadm rollback 报错） | 按 §3.1 人工 cp 兜底；仍失败联系研发值班 |
| 镜像回退失败（digest 不可达） | 检查镜像仓库连通性；v0.1.0 本地归档（release/v0.1.0/）可重建镜像 |

**回滚失败兜底**：`docs/UPGRADE.md §3` 异常路径表 + 备份保留 + 人工 cp 路径（§3.1）。
原则：备份目录**只增不删**（reset 不清理 backups/），回滚失败后备份目录保留现场，不丢失恢复路径。

---

## 7. 回滚后处理

1. **复盘**：24h 内输出复盘报告（触发条件、回滚过程、根因、改进项），模板见 DRILL-SCHEDULE.md §7。
2. **根因**：排查 v0.1.1 修复引入的回归，按 P0/P1/P2 分级，修复后重新走 release-prep-v011.md §4 门禁。
3. **台账**：RELEASE-LEDGER.md §5 登记回滚异常；ops-ledger 全链路可查。
4. **发布门禁**：根因未闭环前，v0.1.1 重新发布需重新走完整发布窗口（release-prep-v011.md §5 从阶段①开始）。

---

*本手册为 v0.1.1 发布保障文件之三；配套 release-prep-v011.md、monitoring-alerting-v011.md。*
*回滚演练脚本为草案——实战前必须由操作人与复核人逐条审核每条命令并确认路径/版本正确。*
*三文件与仓库既有 docs/UPGRADE.md、docs/MULTIARCH.md、docs/DEPLOYMENT.md 互补，不替代。*