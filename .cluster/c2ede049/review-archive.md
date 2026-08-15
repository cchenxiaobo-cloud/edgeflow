# 复核结论存档（重建版）

> 原 review_code.md / review_docs.md 随工作台中间文件被清理；本文件为结论固化重建，
> 内容与 DELIVERY/TEST-REPORT-20260815.md §3、提交链、docs/ 修正提交一致。
> 复核执行人：代码风险复核员（runId a3b6dc70）、文档一致性复核员（runId 96350c4a）。

## 1. 代码风险复核（审 37fbaf4→5ac07f8 六提交九任务）

| 级别 | 项 | 结论 | 处置 |
|------|----|------|------|
| 🔴 阻断 B1 | gzip 协商链断裂：全仓非测试代码无一处给 Compression 赋值，云端协商恒 false，压缩真实部署永不启用 | 功能失效（明文回落完好，无兼容回归） | ✅ 已修复 `5ac07f8`（edge register() 声明 `Compression:"gzip"` + 测试断言增强 + 反向用例保留） |
| 🟡 中 M1 | MarkOffline 重复调用刷新 offlineSince，TTL 时钟重置 | 记录即可 | ✅ 已修复 `8f1bc80`（仅首次转离线记录）+ TestMarkOfflineTwiceKeepsClock |
| 🟡 中 M2 | 未注册连接可发压缩帧触发有界解压 | 容忍式设计，补注释 | ✅ `8f1bc80` dispatch 安全注释（WS 1MiB+解压 1MiB 双限、无放大、token 缓解建议） |
| 🟢 低 L1-L7 | 等待连接 goroutine 存活/SIGHUP stop 语义/rotate 双 rename/备份 id 并发/GC O(n)/swapPort 静默/周期生效延迟 | 不阻塞 | 📋 入档 TEST-REPORT §4 |

基线核查项（M1C P2-1/P2-2、SetReadLimit、Memory 类型、测试存在性）全部确认。

## 2. 文档一致性复核（审 5 份文档）

| 分类 | 数量 | 处置 |
|------|------|------|
| 🔴 必须修正 F1-F10 | 10 | ✅ 全部修正 `09e4707`（热重载/压缩/轮换状态列、Deliver 方向、端点 13→11、压测新基线、KEADM --token、ROADMAP §1.1/§1.2、PROGRESS 回写） |
| 🟡 建议核对 S1-S9 | 9 | ✅ 随附修正（锚点、G-11 内存仅 Linux、G9→G-6 交叉引用、env 全名、3→2 边缘节点、ServiceBus 🔒、MULTIARCH 表述、PROGRESS §6、keadm 描述） |
| 🟢 无误抽查 | 12 组 | ✅ 通过 |

核心根因：文档快照锚定 f6b4898 导致 §12 缺口表与同提交 ROADMAP §8 矛盾——已统一为最新 head 口径。
