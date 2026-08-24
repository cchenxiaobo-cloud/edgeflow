# EdgeFlow 收尾审计复核报告（audit-review）

> **复核人**：收尾复核审计员（独立子代理，depth 1/1）
> **复核时间**：2026-08-14 20:26-20:50 (Asia/Shanghai)
> **复核对象**：`docs/audit-m02.md`（M0-M2 核对）＋ `docs/audit-m35.md`（M3-M5 核对）
> **性质**：只读交叉验证。未改动任何代码/文档（本报告为唯一写入物）。
> **方法**：对两份报告的 10 项关键缺口结论逐项独立取证（代码实读/grep/文件存在性/git log），**不采信报告原文**，全部以实际命令输出佐证。

---

## 1. 逐项复核表

| # | 关键结论（原报告） | 独立验证方法 | 独立结果（实测证据） | 与原报告一致 |
|---|-------------------|-------------|----------------------|:---:|
| 1 | **6.4 镜像更新滚动策略未实现**（audit-m02 §2.3）：EnsureRunning 对已运行容器 no-op，镜像漂移不触发重建，无滚动/回滚 | 实读 `edge/pkg/edged/docker_runtime.go` L140-185 | `grep` 定位 L147 `EnsureRunning`；L152-158 switch：`StateRunning → return nil`，函数头注释明写"已运行 → no-op"；`docker run` 仅出现在 `StateAbsent` 分支，**无任何镜像比对/重建逻辑**；全仓无滚动/回滚策略代码 | ✅ **一致**（注：报告引用行号 L143-146，实测 no-op 分支在 L152-158，行号略偏，结论正确） |
| 2 | **2.8 NodeJob 未实现**（audit-m02 §2.1）：仅协议占位"待定" | 全仓 grep `NodeJob`（go/yaml/md） | 仅 2 处命中，均为 `pkg/protocol/message.go` L24-25 消息类型枚举占位 `// 云→边：任务分发（待定）`；无控制器/CRD/API/提交 | ✅ **一致** |
| 3 | **8.3 E2E 完整场景未做**（audit-m02 §2.4）：无多节点/故障恢复/自治 30min 套件 | 查 tests/ 目录、全仓 e2e/bench 文件、PROGRESS 待办 | 无 `tests/` 目录；`hack/` 仅有 modbus-e2e、edged-smoke、eventbus-smoke、mock-cloudhub 等**专项冒烟脚本**（非 8.3 场景套件）；PROGRESS.md L283/L499 明列"8.3 E2E 完整场景"为未关闭待办 | ✅ **一致** |
| 4 | **10.1 可观测性（Prometheus）未做**（audit-m35 G2）：无指标端点、无 Prometheus/Fluent Bit | 全仓 grep `prometheus/metrics/fluent`（go 非测试代码） | **零命中**；仅 `cmd/cloudcore/main.go` L210 `/healthz` 健康检查端点；无指标导出 | ✅ **一致** |
| 5 | **7.2 RBAC / 7.3 设备认证 / 7.5 审计日志未做**（audit-m35 §3）：只做了 7.1+7.4 | grep `rbac/authz`、`audit`（go 非测试）；读 `cmd/keadm/join.go`；反向确认 mTLS 存在 | `rbac/authz` **零命中**；`audit` **零命中**；join.go L19-20/L58：token"当前版本 edgecore **尚未消费**，预留做后续鉴权"；反向验证 7.1/7.4 有代码：`pkg/certs/certs.go` + `cloud/pkg/cloudhub/server.go` L12/L215 `RequireAndVerifyClientCert` 强制双向认证 | ✅ **一致** |
| 6 | **8.4 性能压测（100 节点）未做**（audit-m35 G3） | 全仓查 stress/loadtest/perf 脚本；grep 验收标准 | 无任何压测脚本/报告；`docs/DEV-ENV.md` L295 自认"100 节点规模需单独脚本（M4 扩展）"；`docs/ROADMAP.md` L265 仅存验收目标（注册 ≥99%/延迟 ≤3s/内存 ≤256MB），无对应证据 | ✅ **一致** |
| 7 | **8.6 keadm 批量/配置迁移未做**（audit-m35 G8/D8）：upgrade/rollback 有，batch/migrate 无 | 读 `cmd/keadm/main.go` 命令注册与 usageText | 子命令全集：init / join / upgrade / rollback / ops-ledger / reset / version——**无 batch、无 migrate**；main.go 注释自称"当前为「基础版」"；upgrade/rollback 文件存在（upgrade.go/rollback.go） | ✅ **一致** |
| 8 | **CRD 无 manifest**（audit-m02 §1.1 #4/#10）：无 kubectl apply 可用的 yaml | 全仓 find `*crd*`；grep `kind: CustomResourceDefinition`（yaml）；查 apis/ | **零命中**：无 config/crd/ 目录、无任何 CRD yaml；`apis/edge/v1alpha1/` 仅 Go 类型文件（types/device/device_model/edge_node/deepcopy/defaults） | ✅ **一致** |
| 9 | **PROGRESS §6 里程碑表滞后**（audit-m02 S2 / audit-m35 D1）：M1-M5 仍标"未开始" | 读 PROGRESS.md §6（L510-521） | §6 表实测：M0 ✅ 完成，**M1-M5 全部"⏳ 未开始"**，与 §3 声明（M1 ✅、M2-M5 🟨 完成）及 §4M 记录（M5 已发布 v0.1.0）直接矛盾 | ✅ **一致** |
| 10 | **ROADMAP §1.2 状态列滞后**（audit-m02 S8 / audit-m35 D4）：WBS 2-10 状态未维护 | 读 ROADMAP.md §1.2 全表 | 实测 WBS **2.1-10.4 全部标"⬜"**（含已完成交付的 2.1 CloudHub、3.1 EdgeHub、4.2 连接管理、4.6 可靠投递、8.5 Helm、9.2-9.5 文档等）；1.x 仅部分 🟨 | ✅ **一致** |

**附加佐证核对（非任务清单项，顺带验证）**：
- git log：**108 commits**，HEAD `eacd35a`，与两份报告基线一致 ✅
- 工作区状态：仅 `docs/audit-m02.md`、`docs/audit-m35.md` 两个 untracked（即报告本身），无其他改动——"只读审计"声明成立 ✅
- keadm 产物实测基准（audit-m35 P1-1）：`release/v0.1.0/keadm-darwin-arm64` 存在；git log 可见 `733d0ae rebuild keadm with P2-4/P2-5 fixes`、`1265214 backfill ledger digests`，与报告所述修复链路吻合 ✅

---

## 2. 误判清单

**无实质性误判。** 10 项关键结论与两份审计报告**全部一致**（10/10）。仅两处非实质性瑕疵，不影响结论：

| 级别 | 位置 | 说明 |
|------|------|------|
| 小瑕疵 | audit-m02 §2.3 | 引用 `docker_runtime.go` 行号 L143-146，实测 no-op 逻辑位于 L152-158（switch 分支体）。行号偏差，结论（已运行不比对镜像）完全正确 |
| 口径 | audit-m02 头部"工作区干净" | 复核时工作区存在 2 个 untracked 文件——即 audit-m02/audit-m35 报告本身。属报告写入后的自然状态，代码零改动，声明实质成立 |

---

## 3. 复核结论

1. **两份报告的"关键缺口"判定全部经独立实证，无一错判或漏判**：
   - 代码层缺口（6.4 滚动策略、2.8 NodeJob、7.2/7.3/7.5、10.1、CRD manifest）均通过源码实读/grep 零命中/占位注释等硬证据确认；
   - 工程活动缺口（8.3 E2E、8.4 压测、8.6 批量/迁移）均通过目录不存在性 + 文档自认 + 待办未关闭三重证据确认；
   - 文档滞后（PROGRESS §6、ROADMAP §1.2）均通过表格原文实测确认。
2. **两份报告彼此交叉一致**：audit-m02 的 S2/S8 与 audit-m35 的 D1/D4 对同一滞后点（PROGRESS 里程碑表、ROADMAP 状态列）的判定相互印证；6.4/8.3 在 audit-m02 中为 P1，audit-m35 中 8.3 继续以 G11 形式跟踪，无矛盾。
3. **报告的严格度评估**：结论偏保守（凡无实证即判"未做/未验证"），未发现将占位/预留/文档声明误判为实现的情况；反向抽查（mTLS 确有代码）亦证明其未误杀已实现项。

**复核统计**：10 项关键结论中 **10 项一致 / 0 项不一致**；误判 0；报告路径 `docs/audit-review.md`。

---

*本复核为只读审计产物，仅写入本文件，未修改任何代码/其他文档。*
