# EdgeFlow 后续开发轮 — 任务台账与进度记录（工作台重建版）

> goal `2bcbad93`（production_ready）；工作台 `.cluster/c2ede049/` 中间文件曾被清理，
> 本文件为可追溯性重建（2026-08-15 22:05），内容与 DELIVERY/ 交付物、git 提交链一致。

## 1. 范围锚定（S1 核准结论）

- 唯一范围依据：`docs/ROADMAP.md`（源自 EdgeFlow-开发计划.md v1.0，2026-08-12 可执行性审查修正）
- "后续开发任务" = ROADMAP 仍标 🟨/⬜ 项 + 四份审查记录（M1B/M1C/M1/M3A）遗留 P2/P3 修复
- 交付形态：代码提交（main）+ 自动化测试 + 测试/质量报告 + 预发布验证记录 + 回滚方案 + 台账交接

## 2. 任务台账（与 git 提交一一对应）

| # | 任务 | ROADMAP/审查 | 状态 | commit |
|---|------|-------------|------|--------|
| N1 | 配置热重载（SIGHUP+60s mtime，端口/周期热切换，fail-safe） | 2.7 | ✅ | `2d0a903` |
| N2 | gzip 消息压缩（Register 协商式双向，EFGZ 帧，1MiB 双限） | 4.4 | ✅ | `37f34f9` |
| N2-B1 | gzip 协商断裂修复（评审阻断项） | 4.4 | ✅ | `5ac07f8` |
| N3 | 存储/注册表审查修复（范围扫描+Offline TTL/GC） | M1B/M3A | ✅ | `37fbaf4` |
| N4 | 通道/API 审查修复（serveWS 竞态/1MiB 限制/newID 兜底+基线核查） | M1C/M1 | ✅ | `d9bc9ec` |
| N5 | keadm 证书轮换（备份先行/事务化重签/幂等） | 7.1 | ✅ | `2c877d4` |
| N6 | keadm 灰度升级分批（--batch-size/--pause-between） | 10.2 | ✅ | `2c877d4` |
| N7 | 架构文档评审回写（9.1） | 9.1 | ✅ | `1ae2d81` |
| N8 | 范围外项处置登记（ROADMAP §8） | — | ✅ | `1ae2d81`+`09e4707` |
| — | 复核处置 M1/M2（离线时钟/安全注释） | 评审 | ✅ | `8f1bc80` |
| — | 文档一致性修正 F1-F10+S 项 | 评审 | ✅ | `09e4707` |
| — | .gitignore 补测试残留目录 | — | ✅ | `2f7a870` |
| — | 交付文档（测试报告/预发布验证/回滚方案/台账） | 交付物 | ✅ | `9a2a2b0` |

提交链：`f6b4898`（基线）→ `9a2a2b0`（HEAD），11 个提交，工作区干净。

## 3. 派单与复核记录（重建摘要）

- 第一轮 4 Agent（存储注册表/通道API/配置热重载/架构文档）：核验通过；W3 超时但实现测试完整，主线核验闭环
- 第二轮 2 Agent（keadm 增强合并 N5+N6、gzip N2）：核验通过（go test -race 全绿、golangci-lint 0 issue）
- 复核 Agent×2：代码风险（1 阻断 B1 已修复+2 中处置+7 低入档）；文档一致性（F1-F10 修正+S1-S9 随附+12 组无误抽查）
- 复核结论存档见本目录 review-archive.md；详细报告原文件被清理，结论已固化进 DELIVERY/TEST-REPORT-20260815.md §3

## 4. 验证证据（可检查）

| 证据 | 位置 | 结果 |
|------|------|------|
| 全量回归日志（最终） | /tmp/ef-final-regression.log | EXIT=0（含 tests/e2e 202.9s） |
| 构建冒烟 | 见 DELIVERY/PRE-RELEASE-VERIFICATION §1 | 三二进制 BUILD-OK |
| cert rotate 实操 | 见 DELIVERY/PRE-RELEASE-VERIFICATION §2.1 | 序列号变化+备份 manifest |
| 交付物 | .cluster/c2ede049/DELIVERY/ ×4 | 齐全 |
