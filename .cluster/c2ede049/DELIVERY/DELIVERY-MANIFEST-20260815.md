# EdgeFlow 后续开发轮 — 任务台账与交接说明

- 轮次：2026-08-15 后续开发轮（goal `2bcbad93`，production_ready）
- 工作台：`.cluster/c2ede049/`（plan.md / subagent_01~07.md / review_code.md / review_docs.md / DELIVERY/）
- 提交链：`f6b4898`（基线）→ `09e4707`（HEAD），10 个提交 / 49 文件 / +5703 -442

---

## 1. 任务台账（与 ROADMAP §8 处置登记一一对应）

| 编号 | 任务 | ROADMAP | 状态 | commit |
|------|------|---------|------|--------|
| N1 | 配置热重载（SIGHUP + 60s mtime，端口/周期热切换，fail-safe） | 2.7 | ✅ 完成 | `2d0a903` |
| N2 | gzip 消息压缩（Register 协商式双向，EFGZ 帧，炸弹防护） | 4.4 | ✅ 完成 | `37f34f9` |
| N2-B1 | gzip 协商断裂修复（评审阻断项） | 4.4 | ✅ 完成 | `5ac07f8` |
| N3 | 存储/注册表审查修复（范围扫描 + Offline TTL/GC + devicestatus 结论） | M1B/M3A | ✅ 完成 | `37fbaf4` |
| N4 | 通道/API 审查修复（serveWS 竞态/1MiB 限制/newID 兜底 + 基线核查） | M1C/M1 | ✅ 完成 | `d9bc9ec` |
| N5 | keadm 证书轮换（备份先行/事务化重签/幂等） | 7.1 | ✅ 完成 | `2c877d4` |
| N6 | keadm 灰度升级分批（--batch-size/--pause-between） | 10.2 | ✅ 完成 | `2c877d4` |
| N7 | 架构文档评审回写（NATS→MQTT/Token→mTLS/进度回写） | 9.1 | ✅ 完成 | `1ae2d81` |
| N8 | 范围外项处置登记（ROADMAP §8 表） | — | ✅ 完成 | `1ae2d81` + `09e4707` |
| — | 复核处置 M1/M2（离线时钟/安全注释） | 评审 | ✅ 完成 | `8f1bc80` |
| — | 文档一致性修正 F1-F10 + S 项 | 评审 | ✅ 完成 | `09e4707` |
| — | .gitignore 补测试数据目录 | — | ✅ 完成 | `2f7a870` |

## 2. 派单与复核记录

- 第一轮 4 个 Agent（存储/注册表、通道/API、配置热重载、架构文档）：全部核验通过；W3 超时但实现/测试完整，主线直接核验闭环（不重派）。
- 第二轮 2 个 Agent（keadm 增强、gzip 压缩）：全部核验通过。
- 复核 2 个 Agent：代码风险（review_code.md：1 阻断已修复 + 2 中 + 7 低入档）、文档一致性（review_docs.md：F1-F10 修正 + S1-S9 随附 + 12 组无误抽查）。
- 全程 subagent 不 commit，主线按 worker 主题分组提交（可追溯、可回滚）。

## 3. 已登记遗留事项（不阻塞交付）

| 项 | 状态 | 说明 |
|----|------|------|
| C7 GitHub 远程关联 + CI 首跑 | 🔒 用户操作 | 环境无远程凭据，需用户执行 `git remote add` 后跑 CI |
| 集群级验收（8.2 多节点 E2E / 30min 长跑 / 100 节点复测） | ⏸ 环境边界 | 需真实集群环境 |
| 10.4 cosign 签名 | ⏸ 环境边界 | 需镜像仓库 + 签名基础设施 |
| 5.2 OPC-UA / 6.5 资源超卖 | ⏳ 延后排期 | ROADMAP §8 已登记 |
| 3.7 ServiceBus / 5.3 K8s 控制器 / Flannel | 🔒 决策关闭 | ROADMAP §8 已登记 |
| L1-L7 低风险 | 📋 入档 | TEST-REPORT §4 |
| 4.4 Protobuf 编码 | ⏳ 延后 | 规模化阶段（gzip 已落地） |

## 4. 交接要点（给下一位开发者/维护者）

1. **gzip 压缩**：默认开启（`compress` 缺省 true，配置变更需重启）；协商闭环 = edge Register 声明 → cloud RegisterAck 回带 → 双向压缩；旧版本端自动明文互操作，无需停机升级。协议细节见 docs/ARCHITECTURE.md §4.6。
2. **配置热重载**：SIGHUP + 60s mtime；cloudcore 端口热切换（失败回滚旧监听）；edgecore 上报周期热生效，cloudAddr/nodeID/reconcileInterval 变更需重启（回写旧值+告警）。
3. **证书轮换**：`keadm cert rotate --node <CN> [--cert-dir]`；每次轮换前自动备份到 `backups/<ts>/`（manifest+sha256）；轮换不自动分发、不自动重启，回退=备份覆盖。
4. **灰度升级**：`keadm upgrade --batch-size N --pause-between 30s`（单节点无分批效果，batch 模式生效）；fail-fast 中止 + 成功/失败清单 + rollback 衔接。
5. **审查修复语义**：metamanager List 为范围扫描（LIKE 通配符字面匹配）；registry Offline 节点 24h 后惰性 GC（`WithOfflineTTL` 可调，≤0 禁用）；重复 MarkOffline 不刷新离线时钟。
6. **验收证据**：测试报告 / 预发布验证记录 / 回滚方案见 DELIVERY/；ROADMAP §8 为处置总登记。

## 5. 假设声明（本轮明确标注）

- "后续开发任务" = ROADMAP 仍标 🟨/⬜ 项 + 4 份审查记录遗留 P2/P3 修复（计划内，不含新方向）。
- 预发布环境 = 本机沙箱（真实进程/命令级验证）；生产集群部署按 DEPLOYMENT 文档执行，本工作台记录为仓库级交付证据。
- 部署开关默认值按 v1.0 兼容优先（gzip 默认开但自动降级；token 默认不强制校验）。
