# EdgeFlow 未完成任务与整改建议清单

> 依据：《产品开发计划收尾核对报告》（docs/CLOSE-OUT-REPORT.md）
> 建议责任人：以角色标注（项目负责人/后端工程师/运维工程师/安全工程师），最终归属需项目负责人确认
> 优先级：P1（影响验收/安全，建议 2 周内）→ P2（功能完整性，建议 1 个迭代内）→ P3（文档/跟踪，建议随下次改动同步）

---

## 一、P1 级（建议 2 周内处置）

| # | 任务 | 建议责任人 | 建议完成时间 | 处置建议 |
|---|------|-----------|-------------|---------|
| ✅ A1 | 8.3 E2E 完整场景套件 | 后端工程师 | 已完成 | tests/e2e 三用例全 PASS（自治 60s 短时模拟/设备链路/多节点隔离），真实进程+真实 Docker，commit `a0a4344`（含 waitContainerGone 根因修复 `ac3913f`）；30min 需真实环境长跑（PERFORMANCE-BASELINE.md） |
| ✅ A2 | 6.4 镜像更新滚动策略 + 回滚 | 后端工程师 | 已完成 | EnsureRunning 镜像比对+漂移重建（批大小 1 逐轮滚动），单测 5 项全过，commit `a0a4344`；回滚经 re-deploy 语义覆盖 |
| ✅ A3 | 10.1 可观测性（/metrics 指标端点） | 后端工程师 | 已完成 | /metrics 五指标（nodes/pods/devices_total、http_requests_total、active_connections），Prometheus 文本格式，metrics 覆盖 96.6%，commit `4c5b9c6`；CPU/内存/日志采集为 P2 |
| ✅ A4 | 7.2 RBAC（Token 认证中间件） | 安全工程师 | 已完成 | API Token 认证中间件（env 开关默认 off 向后兼容），常数时间比较+401 审计留痕，auth 覆盖 94.1%，commit `4c5b9c6`；角色分级为 P2 |
| ✅ A5 | 7.5 审计日志（操作审计） | 安全工程师 | 已完成 | audit-ledger.jsonl 审计台账（JSONL 追加写、失败不阻断 API、启动期 fail-fast），audit 覆盖 76.3%，commit `4c5b9c6`；查询 API 为 P2 |
| ✅ A6 | 8.4 性能压测（节点模拟） | 后端工程师 | 已完成 | hack/load-test：10 节点 100% 注册、平均 201ms、P95 202ms，commit `a0a4344` + PERFORMANCE-BASELINE.md；100 节点需集群环境（已知限制） |
| ✅ A7 | 真实集群验证（kind） | 项目负责人 + 运维 | 已完成 | kind 集群 CRD apply + keadm init + edgecore 注册 Ready 全链路跑通（15min 验收满足），REAL-CLUSTER-GUIDE.md 记录，commit `b809e30`；正式环境演练排期待确认 |

## 二、P2 级（建议 1 个迭代内处置）

| # | 任务 | 建议责任人 | 处置建议 | 状态（2026-08-15 收尾轮） |
|---|------|-----------|---------|--------------------------|
| B1 | 7.3 设备认证（Token 消费） | 安全工程师 | join 生成的 token 由 edgecore 注册时携带，云端校验 | ✅ 已完成：Register.token 云边双向 + EDGEFLOW_CLOUDCORE_NODE_TOKEN 校验（常数时间比较），单测 6 项 + hack/token-auth-check.sh 真实进程验证（正确 token 注册成功/错误/缺失被拒且无注册表污染） |
| ✅ B2 | 2.8 NodeJob 明确处置 | 项目负责人 | 已关闭（v0.1.0 范围外产品决策）：协议占位标注"已关闭"，commit `4c5b9c6` | ✅ 维持 |
| B3 | keadm 批量操作 | 后端工程师 | batch join/upgrade（读清单文件逐节点执行） | ✅ 已完成：keadm batch 子命令（join/upgrade/rollback，清单逐行，复用单命令逻辑），单测 5 项 + 真实运行验证 2 节点批量 join |
| B4 | CRD manifest yaml 生成 | 后端工程师 | 手写或脚本生成 CustomResourceDefinition yaml（apis 类型已有） | ✅ 已完成（b809e30）：config/crd/ 3 个 CRD（edgenodes/devicemodels/devices.edgeflow.io）字段与 Go 类型一致，A7 kind 集群已 apply |
| ✅ B5 | 多架构发布镜像 | 运维工程师 | 已完成：本地 registry 双架构重建（buildx manifest 确认 + 4×--version 一致），commit `b809e30`；远程推送需 registry 凭据（环境边界） | ✅ 维持 |
| B6 | 10.4 镜像安全扫描 | 运维工程师 | trivy/grype 扫描 release 镜像；记录结果到台账 | ✅ 已完成：Trivy 0.74.0 扫描两镜像 0 漏洞；首扫发现 golang.org/x/net v0.44.0 10 个漏洞（5H+5M）→ 升级 v0.56.0 → 重建镜像 → 复扫 0 漏洞；记录 docs/SECURITY-SCAN.md |
| B7 | 10.3 API 兼容矩阵 | 后端工程师 | 文档化支持端点/字段版本矩阵 | ✅ 已完成：docs/API-COMPATIBILITY.md（13 REST 端点 + 9 云边消息类型 + 3 CRD 类型 + 变更登记） |
| B8 | 跨主机 CA 分发自动化 | 安全工程师 | gen-certs 输出分发包或文档化分发步骤 | ✅ 已完成：gen-certs.sh 支持 CERT_DIST_DIR 分发包（cloud/ + edge/<CN>/，含 README 部署说明），实测 openssl verify 链验证通过 |

## 三、P3 级（文档与跟踪，随下次改动同步）

| # | 任务 | 处置建议 |
|---|------|---------|
| ✅ C1 | ROADMAP.md §1.2 状态列同步 | 已回写 14 处（WBS 2/6/7/8/10 + 2.8/2.9/3.8/6.4/7.2/7.5/8.3/8.4/10.1），commit `a0a4344`+`4c5b9c6` 对应实现 |
| ✅ C2 | PROGRESS.md 里程碑表同步 | 已回写 6 处（§5 待办 P1 关闭 + M2/M4 验收达成），commit `b809e30`+本轮 |
| C3 | 待办清单清理 | ✅ 已完成（2026-08-15）：§5 待办 7.3/10.3/batch/CA 分发/扫描/arm64 二进制 6 项已闭环回写（PROGRESS.md） |
| ✅ C4 | KEADM.md 标题与内容 | 已修正，commit `b809e30` |
| ✅ C5 | Release Notes / 台账时间线 | 已回填，commit `b809e30` |
| C6 | 里程碑归属偏移回写 | ✅ 已完成（b809e30）：ROADMAP §3 注已标注 2.4/4.5/5.2/8.6 实际完成里程碑 |
| C7 | GitHub 远程关联 + CI 首跑 | ⏸ 需用户操作：SSH 公钥已存在（~/.ssh/id_ed25519.pub），无 git remote；步骤：GitHub 添加公钥 → 提供仓库地址 → `git remote add origin <url>` → `git push -u origin main` → 确认 Actions lint+test 绿勾 |

---

## 四、优先级排序依据

- P1 = 直接影响 M1/M2/M4/M5 验收达成或安全基线（E2E 套件、滚动策略、可观测性、认证、压测、真实集群）
- P2 = 功能完整性/发布质量（不影响核心链路运行）
- P3 = 文档与跟踪（不阻塞功能，影响可追踪性与交接效率）

> 注：本清单为建议，最终责任人与排期需项目负责人确认（符合收尾检查"不代替模块负责人签字"边界）。
