# EdgeFlow v0.1.1 发布准备清单（Release Prep）

> 状态：✅ 已入档（2026-08-18；发布准备员起草，报告核验员核验口径一致，主线修正 3 处：checksums 12 条目、36 包口径、灰度走 keadm batch）｜随代码合入进展回填勾选。
> 性质：**发布保障文档**——v0.1.1 发布闭环的检查清单与执行顺序，不替代
> `docs/RELEASE-CHECKLIST.md`（逐制品复核方法），两者配套使用。
> 依据：docs/RELEASE-CHECKLIST.md、docs/PERFORMANCE-BASELINE.md、docs/SECURITY-SCAN.md、
> docs/DEPLOYMENT.md、docs/SECURITY.md、docs/ARCHITECTURE.md（§10 部署形态 / §12 残留缺口）、
> docs/PROGRESS.md §5（Backlog）、docs/UPGRADE.md、docs/MULTIARCH.md、docs/KEADM.md。

---

## 1. 版本决策

| 项 | 决策 | 理由 |
|----|------|------|
| 版本号 | **v0.1.1（patch）** | SemVer：仅安全加固 + P2 收尾，无破坏性变更、无新 API |
| 产品基线 | v0.1.0（不可变 tag，发布后禁止改写） | 沿用 RELEASE-CHECKLIST.md 口径 |
| 升级路径 | v0.1.0 → v0.1.1 直接升级 | SQLite 跨小版本兼容（DEPLOYMENT.md §5.2）；协议信封 `version` 字段不变 |
| 兼容承诺 | API 端点/消息类型/配置项**只增不改** | docs/API-COMPATIBILITY.md、ARCHITECTURE.md §4.3（枚举开放集合） |
| 发布范围 | 3 项审计中等风险 + P2 遗留收尾（§2） | 不含新功能；新功能进 v0.2.0 |

**Go/No-Go 总闸**：§4 检查项全部通过 + §5.4 观察期无 P0/P1 → 才允许全量发布。
任何一项失败 → 修复后重新走 §4，不得带病发布。

---

## 2. 发布范围清单（v0.1.1 变更内容）

> ⚠️ 以下为**计划修复项**，最终以实际合入 main 的 commit 为准；发布前由发布人
> 用 `git log v0.1.0..v0.1.1 --oneline` 与下表核对，差异写入 §2.3。

### 2.1 审计中等风险修复（安全加固，本轮主目标）

| # | 修复项 | 说明 | 关联背景 |
|---|--------|------|---------|
| S1 | **/ocsp 限流** | POST /ocsp（RFC 6960 responder）增加请求速率限制，超限返回 429 | OCSP responder 为 2026-08-16 闭环（commit `5960bda`）；限流防未认证端点被滥用 |
| S2 | **OCSP 新鲜度校验** | 客户端侧校验 OCSP 响应 freshness（thisUpdate/nextUpdate 窗口），防重放旧响应；**客户端库能力已提供（WithPolicy 入口），生产路径尚未接线（边缘 mTLS 握手仍走 CRL）——本项为库能力加固** | ARCHITECTURE.md §12 G-6 残留：「OCSP 客户端库 API 未接入生产路径」 |
| S3 | **CRL 降级日志** | mTLS 握手按 CRL 拒绝已吊销证书；CRL 加载/校验失败时输出明确 Warn 降级日志（区分「按 CRL 拒绝」与「CRL 不可用放行/降级」两种路径） | SECURITY.md §5：吊销闭环 2026-08-16 已实现；降级行为必须可观测（配合 monitoring-alerting-v011.md 日志告警） |

### 2.2 P2 遗留收尾（审查项清零）

| # | 修复项 | 出处 | 说明 |
|---|--------|------|------|
| P1 | **WriteTimeout 缺失** | docs/CODE-REVIEW-M1B.md（P2 项×9） | HTTP server 补 Read/Write 超时，防慢客户端占连接 |
| P2 | **ReliableSendContext** | docs/CODE-REVIEW-M1C.md（P2 项×5，`8321b0e` 已修部分） | ReliableSend 接入 context 取消，收尾未修项 |
| P3 | **byDevice namespace** | docs/CODE-REVIEW-M3A.md（P2 项×6，PROGRESS.md §5 待办行） | byDevice 路由补齐 namespace 维度，避免跨命名空间设备冲突 |
| P4 | **LastReportedAt 单调性** | docs/CODE-REVIEW-M3A.md（P2 项×6） | 设备上报时间戳单调化，防时钟回拨导致的影子乱序 |
| P5 | **广播送达计数日志**（已在改，未提交） | cloud/pkg/cloudhub/router.go 工作区 diff | Deliver 广播分支补「送达 N 个节点」Info 日志（P2-4 广播尽力而为语义文档化） |

> 其余 P2 项（如 M3A「多实例 Mapper」「空 deviceName 校验」等）若本轮一并合入，
> 在 §2.3 追加记录；未合入项保持 PROGRESS.md §5 Backlog 跟踪。

### 2.3 实际合入核对（发布时回填）

```bash
cd /Users/mac/Documents/edgeflow
git log v0.1.0..HEAD --oneline   # v0.1.1 tag 前回填
```

| 实际合入 commit | 对应上表条目 | 备注 |
|-----------------|-------------|------|
| `bc8994d` | S1 / S2 / S3 | 审计 3 中风险修复（限流/新鲜度/CRL 降级日志）|
| `59dd396` | P1 / P2 / P3 / P4 / P5 | P2 遗留闭环（含 router.go P2-4，归属第一轮 p2-closure）|
| `20e66b5` | §2.3 文档 | API-SPEC/SECURITY/CODE-REVIEW/PROGRESS/ROADMAP 同步 |
| `e6527d9` / `92be18c` | §2.3 文档 | CLOSE-OUT-ACTIONS §5 / CLOSE-OUT-RISKS 登记 |
| `366a5ce` | §2.3 文档 | 用户手册 v0.1.2 + 解决方案手册 v1.0.2 四制品同步 |

> 核对结论（发布轮）：`git log v0.1.0..HEAD --oneline` 与上表一致，无遗漏变更；制品构建基线 = 制品重建员实际构建时的 HEAD（构建时回填）。

---

## 3. 制品清单（release/v0.1.1/）

> 布局与 v0.1.0 一致（见 `ls release/v0.1.0/`）；**注意**：v0.1.0 发布清单初版口径为
> 6 二进制（2 平台），2026-08-15 补齐 linux-arm64 后实际为 **9 二进制**（ARCHITECTURE.md §10）。
> v0.1.1 沿用 9 制品布局。

### 3.1 二进制（9 个，3 组件 × 3 平台）

| 组件 | darwin-arm64 | linux-amd64 | linux-arm64 |
|------|--------------|-------------|-------------|
| cloudcore | cloudcore-darwin-arm64 | cloudcore-linux-amd64 | cloudcore-linux-arm64 |
| edgecore | edgecore-darwin-arm64 | edgecore-linux-amd64 | edgecore-linux-arm64 |
| keadm | keadm-darwin-arm64 | keadm-linux-amd64 | keadm-linux-arm64 |

版本注入：`--version` 输出 `version=v0.1.1 gitCommit=<sha> buildTime=<ts> goVersion=go1.x`（keadm 用 `version` 子命令）。

### 3.2 其余制品

| 制品 | 要求 |
|------|------|
| `edgeflow-0.1.1.tgz` | `helm show chart` → version=0.1.1、appVersion=v0.1.1；values.yaml 与 `build/charts/edgeflow/values.yaml` 无 diff |
| `checksums.txt` | 覆盖 **12 个制品**（9 二进制 + 1 Chart 包 + images.json + sbom.json，同 v0.1.0 口径），`shasum -a 256 -c` 全 OK |
| `sbom.json` | 组件清单与 `go list -m all` 一致；artifacts[].sha256 与 checksums.txt 对应 |
| `images.json` | cloudcore/edgecore v0.1.1 双架构（amd64+arm64）digest/size/arch，digest 为推送后真实值 |
| 镜像 | `<registry>/edgeflow/cloudcore:v0.1.1`、`edgecore:v0.1.1`，双架构 manifest（`docker buildx imagetools inspect` 断言两平台） |

### 3.3 制品来源命令（参考 v0.1.0 路径）

```bash
make build && make cross-build            # 二进制（dist/ → release/v0.1.1/）
helm package build/charts/edgeflow -d release/v0.1.1 --version 0.1.1 --app-version v0.1.1
docker buildx build --platform linux/amd64,linux/arm64 \
  -f build/docker/Dockerfile --target cloudcore -t <reg>/edgeflow/cloudcore:v0.1.1 --push .
# edgecore 同；digest 以 push 返回值为准回填 images.json（RELEASE-CHECKLIST.md 远程发布步骤）
```

---

## 4. 发布前检查项（全部通过才可打 tag）

### 4.1 代码质量

- [ ] **全量回归 `go test -race ./...` 全绿**（36 包口径，含新增修复项测试）
- [ ] **`golangci-lint run` 0 issues**
- [ ] **覆盖率 ≥ 70%**（v0.1.0 口径 77.8%，docs/PROGRESS.md §4N；不得显著回落）
- [ ] 新增修复项均有对应单测（S1 限流 / S2 新鲜度 / S3 降级日志 / P1-P4）

### 4.2 安全扫描（docs/SECURITY-SCAN.md 口径）

- [ ] `trivy image --exit-code 1 --severity HIGH,CRITICAL` 对 cloudcore/edgecore v0.1.1 镜像**0 高危**
- [ ] `govulncheck ./...` 无已知漏洞（golang.org/x/net 依赖看护，v0.1.0 曾因 x/net v0.44.0 检出 10 漏洞）
- [ ] 修复项自身安全回归：/ocsp 限流绕过测试、OCSP 重放拒绝测试、CRL 降级路径测试

### 4.3 制品一致性

- [ ] `helm lint build/charts/edgeflow` 0 failed；`helm template` 渲染检查（镜像/探针/env/资源）
- [ ] **OpenAPI 一致性**：`bash hack/gen-openapi.sh` 重新生成无 diff（docs/openapi/edgeflow-openapi.yaml 为自动生成物）
- [ ] **契约测试**：API 兼容性契约测试（`6091f4f`）+ 运行时反向探测（`a109135`）通过；端点清单与 docs/API-SPEC.md（14 端点口径）一致
- [ ] checksums 复验 10/10 OK；sbom.json `python3 -m json.tool` 合法、组件数 ≥ 33

### 4.4 镜像

- [ ] 双架构 manifest 自检：`docker buildx imagetools inspect` 断言 amd64/arm64 均存在（CI release.yml 同款自检）
- [ ] digest 指向内容可运行：`docker run --rm <reg>/edgeflow/cloudcore@sha256:<digest> --version` = v0.1.1
- [ ] 推送后远程 digest 与 images.json 一致（远程仓库 API 复核）

### 4.5 功能回归（发布前冒烟）

- [ ] `bash examples/demo.sh` 一键端到端 DEMO PASS（含注册/Pod/设备/MQTT 链路）
- [ ] 性能基线重跑：`hack/load-test` N=10（注册/心跳 100%、P99 与 docs/PERFORMANCE-BASELINE.md 基线同量级）
- [ ] keadm 演练：`keadm upgrade --version=v0.1.1 --simulate-failure` 预期失败 exit=1 → `keadm rollback --latest` 恢复（UPGRADE.md §5）
- [ ] 证书链路冒烟：`keadm cert rotate` / `keadm cert revoke` + OCSP responder 互操作（`hack/gen-certs.sh`、`hack/token-auth-check.sh`）

### 4.6 文档

- [ ] Release Notes（v0.1.1 变更条目 + 修复项对应表）成稿
- [ ] docs/RELEASE-LEDGER.md 预留 v0.1.1 区块（制品清单/digest/核验人/日期）
- [ ] 用户手册如需随修复项更新，标注修订版本并重新产出 PDF（docs/manual/，v0.1.1/v1.0.1 修订口径见 commit `e0c27cd`）

---

## 5. 发布窗口与顺序

| 阶段 | 动作 | 出口条件 |
|------|------|---------|
| **① 预发（staging 冒烟）** | 本机/测试环境跑 §4.5 全套冒烟；监控指标接入本地 Prometheus（见 monitoring-alerting-v011.md §5） | 冒烟全绿，告警规则无噪 |
| **② 制品构建** | §3 制品全量产出 + 校验 + 打 tag `v0.1.1`（不可变） | §4.1-4.4 全勾选 |
| **③ 灰度分批** | `keadm batch --op=upgrade --file=<清单> --version=v0.1.1 --batch-size=1 --pause-between=<N>`（KEADM.md 灰度参数，2026-08-15 闭环；单节点场景亦可 `keadm upgrade --batch-size=1`）；cloudcore 经 `helm upgrade --install -f values-prod.yaml --set cloudcore.image.tag=v0.1.1` | 首批节点 Ready、无异常 |
| **④ 观察** | 观察期（建议 ≥24h 或与运维确认窗口，参照 DRILL-SCHEDULE.md §1 排期模式）；盯 monitoring-alerting-v011.md 告警静默 + PERFORMANCE-BASELINE 指标平稳 | 无 P0/P1、注册成功率 100%、无新告警 |
| **⑤ 全量** | 剩余批次全量升级（batch 逐节点）；镜像全节点替换 | 全部节点 v0.1.1 |
| **⑥ 台账回填** | RELEASE-LEDGER.md 回填制品清单/digest/核验人/日期；发布通告 | 台账完整可查 |

**灰度边界**：升级批次间 `--pause-between` 用于观察；任何批次失败 → 立即停止后续批次，
执行 rollback-runbook-v011.md §3 ①（`keadm rollback --latest`），并升级 §2.3 差异核对。

**回滚兜底**：全程回滚预案见 `rollback-runbook-v011.md`（同批产物）。

---

## 6. 发布门禁决策表（Go/No-Go）

| 信号 | 处置 |
|------|------|
| §4 任一检查项未过 | **No-Go**：修复重跑 |
| 灰度批次出现注册失败 / 状态异常 | **No-Go 该批次**：回滚 + 根因分析 |
| 观察期出现 P0/P1 告警 | **No-Go 全量**：按 rollback-runbook-v011.md 处置 |
| 观察期满、指标平稳、台账齐备 | **Go**：全量 + 台账回填 |

---

## 7. 与既有文档的引用关系

| 本文档环节 | 既有文档依据 |
|-----------|-------------|
| 制品逐项复核方法 | docs/RELEASE-CHECKLIST.md（v0.1.0 版，v0.1.1 沿用其验证命令） |
| 修复项来源 | docs/CODE-REVIEW-M1B.md / M1C / M3A + docs/PROGRESS.md §5 |
| 安全口径 | docs/SECURITY.md §5（吊销闭环/OCSP/CRL）、docs/SECURITY-SCAN.md（trivy 门禁） |
| 升级回滚机制 | docs/UPGRADE.md（keadm upgrade/rollback）、docs/KEADM.md |
| 镜像双架构与回退 | docs/MULTIARCH.md（§6 指回旧 digest） |
| 性能基线 | docs/PERFORMANCE-BASELINE.md（回归对比基准） |
| 演练排期 | docs/DRILL-SCHEDULE.md（生产演练窗口，仍【需确认】） |
| 部署细节 | docs/DEPLOYMENT.md §2/§5 |

---

*本清单为 v0.1.1 发布保障文件之一；配套：monitoring-alerting-v011.md（监控告警）、
rollback-runbook-v011.md（回滚手册）。三者与仓库既有 docs/ 文档链互补，不替代。*
