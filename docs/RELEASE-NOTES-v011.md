# EdgeFlow v0.1.1 Release Notes

> 版本：v0.1.1（patch）｜发布日期：2026-08-18｜仅安全加固 + P2 收尾，无破坏性变更、无新 API。
> 配套：`docs/RELEASE-LEDGER.md`（v0.1.1 区块）、`docs/RELEASE-PREP-v011.md`、`docs/MONITORING-ALERTING-v011.md`、`docs/ROLLBACK-RUNBOOK-v011.md`。

---

## 1. 发布概述

v0.1.1 是 v0.1.0 之后的第一个 patch 版本，闭环 2026-08-15 收尾审计发现的 3 项中等安全风险与历次代码审查遗留的 P2 项。API 端点（14 个）、消息协议、配置项全部**只增不改**，v0.1.0 → v0.1.1 可直接升级。

| 项 | 内容 |
|----|------|
| 版本号 | v0.1.1（Chart 0.1.1 / appVersion v0.1.1） |
| 发布基线 | `92be18c`（制品构建时 HEAD）；tag `v0.1.1`；归档 commit `94bde2e` |
| 升级路径 | v0.1.0 → v0.1.1 直接升级（keadm upgrade / keadm batch 灰度 / helm upgrade） |
| 兼容承诺 | docs/API-COMPATIBILITY.md 口径不变：端点/消息/配置只增不改 |

## 2. 变更清单

### 2.1 安全加固（审计 3 项中等风险，commit `bc8994d`）

| # | 变更 | 说明 |
|---|------|------|
| S1 | POST /ocsp 限流 | per-IP 令牌桶（默认 10 req/s、burst 20），超限 429；`EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT` 可调；成功响应带 `Cache-Control: max-age=3600` |
| S2 | OCSP 新鲜度校验 | 客户端库新增 `OCSPStatusAtWithPolicy` / `ParseOCSPResponseWithFreshness`（fail-closed：过期/未来时间拒绝，默认 5min skew，ErrOCSPResponseStale/FutureTime）；旧入口行为不变。**库能力已提供，生产路径尚未接线**（当前边缘 mTLS 握手走 CRL）；接线时须用 WithPolicy 入口 |
| S3 | CRL 锁降级可观测 | 锁获取失败自动降级无锁校验（功能语义不变）+ 5 分钟限频 Warn 日志 |

### 2.2 P2 遗留闭环（commit `59dd396`）

| # | 变更 | 出处 |
|---|------|------|
| P1 | HTTP server WriteTimeout=15s（防慢客户端占连接） | M1B P2-5 |
| P2 | Encode 失败日志 17 处（logEncodeError） | M1B P2-6 |
| P3 | Broadcast 送达计数 Info 日志 | M1B P2-4 |
| P4 | ReliableSend context 取消 + 原子性核验 | M1C P2-3/4 |
| P5 | Route 注册增加 namespace（防跨命名空间设备冲突） | M3A P2-1 |
| P6 | LastReportedAt max() 单调保护（防时钟回拨乱序） | M3A P2-4 |
| P7 | downlinkMu 并发保护核验 + 补测 | M1C 核验项 |

其余 P2 项按复核结论以代码注释记录处置口径，见 CODE-REVIEW-M1B/M1C/M3A.md 处置记录。

### 2.3 文档同步

- API-SPEC：/ocsp 行补 429+Cache-Control、错误码表补 429、新增 §1.3 认证与限流小节（`20e66b5`）
- SECURITY：新增「吊销闭环加固 2026-08-18 v0.1.1」条目（`20e66b5`）
- CODE-REVIEW M1B/M1C/M3A 处置记录、PROGRESS §5、ROADMAP §9（`20e66b5`）
- CLOSE-OUT-ACTIONS §5（D1-D10 登记）、CLOSE-OUT-RISKS 风险降级记录（`e6527d9`/`92be18c`）
- 用户手册 v0.1.2 + 解决方案手册 v1.0.2：/ocsp 限流+缓存+新鲜度+CRL 降级日志入册，四制品同步（`366a5ce`）
- 发布保障三件套入档：Release Prep / 监控告警（11 条规则+通知矩阵）/ 回滚手册（`3bb40f2`）
- RELEASE-LEDGER v0.1.1 区块（`e7b64e9`）

## 3. 验证记录

| 项 | 结果 |
|----|------|
| 全量回归 `go test -race ./...` | 全绿（36 包，含 e2e） |
| build / vet / gofmt | 通过（gofmt 0 diff） |
| 异常路径演练 | 14 条：13 ✅ / 1 ⚠️（rotate 连跑两次序列号真实轮换=设计语义）/ 0 ❌ |
| 预发冒烟 | ✅ 签核通过：基础链路 + 429 精确触发（burst=20 第 21 发 429）+ Cache-Control + Stale 断言 + CRL 降级日志 + Docker 链路 |
| 代码复核 | 有条件通过（High）：0 blocker / 0 major / 4 minor / 6 nit，minor 全部登记处置 |
| 报告核验 | 高置信通过：8 条低严重度口径不一致已全部修正 |

## 4. 制品（release/v0.1.1/，12 文件 + 2 镜像）

| 制品 | sha256 前 12 位 |
|------|----------------|
| cloudcore × 3 平台 | 38e21ff1a1a7 / aad536661ac7 / 34995f31f742 |
| edgecore × 3 平台 | 3aa52bf53fd5 / 4ed9c0ffdb99 / ac6520c6a80a |
| keadm × 3 平台 | 3de0ed893aa3 / af74a2cafc63 / eab4f9936545 |
| edgeflow-0.1.1.tgz | 602b91cae1c1 |
| images.json | f10cb45e7f85 |
| sbom.json | bfd98a8778ca |

- checksums.txt：12 条目，`shasum -a 256 -c` 12/12 全 OK（复验通过）
- 双架构镜像（localhost:5001）：cloudcore index `sha256:9f6c8edf…`、edgecore index `sha256:89adb80c…`（linux/amd64 + linux/arm64，--provenance=false）
- 二进制 --version：9 个均 `version=v0.1.1 gitCommit=92be18c`
- trivy（HIGH,CRITICAL）：双镜像 0 漏洞（debian 12.15 + gobinary 均 0）
- docker run 双平台验证：amd64/arm64 × 2 镜像 --version 输出一致（arm64 模拟可运行）
- 制成品冒烟：19080/12000 非默认端口 healthz 200 → edgecore 注册 Ready

**观察项**：镜像内 go1.26.6 vs 本机二进制 go1.26.2（行为一致）；trivy 漏洞库 2026-08-15（库龄 3 天）；远程推送与 kind 集群实测为环境边界（与 v0.1.0 同口径）。

## 5. 升级与回滚

- 升级：`keadm upgrade --version=v0.1.1`；灰度分批走 `keadm batch --op=upgrade --file=<清单> --batch-size=1 --pause-between=<N>`；cloudcore 走 helm upgrade（见 docs/RELEASE-PREP-v011.md §5）
- 回滚：`keadm rollback --latest`；四类场景处置见 docs/ROLLBACK-RUNBOOK-v011.md
- 监控：11 条告警规则 + 通知矩阵见 docs/MONITORING-ALERTING-v011.md（/ocsp 429 复用现有 http_requests_total 分桶，不加新指标；阈值待灰度观察期实测校准）
