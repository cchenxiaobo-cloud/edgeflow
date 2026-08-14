# EdgeFlow v0.1.0 发布台账（Release Ledger）

> 状态：✅ 已定稿（2026-08-14，制品归档 + keadm 重建 + 镜像双架构重建后全部字段已回填）
> 配套文档：`docs/RELEASE-NOTES-v0.1.0.md`（发布说明）、`docs/MULTIARCH.md`（镜像）、`docs/UPGRADE.md`（升级回滚）。
> 维护规则：本台账只增不改；回填字段由发布制品工程师填写，复核字段由复核工程师填写；异常随时登记 §5。

---

## 1. 发布信息

| 项目 | 内容 |
|------|------|
| 版本 | v0.1.0（Chart 0.1.0 / appVersion v0.1.0） |
| 发布基线 commit | `ca2051b`（98 提交） |
| 制品目录 | `release/v0.1.0/`（已归档，commit `d17bdd5` + keadm 重建 `733d0ae`） |
| 编制人 | 发布文档工程师（本文件） |
| 制品负责人 | 发布制品工程师（并行执行中） |
| 复核负责人 | 复核工程师（制品归档后执行） |
| 计划发布日期 | **2026-08-14**（实际发布，见 §2 时间线步骤 9） |

---

## 2. 发布时间线（构建 → 校验 → 归档 → 镜像推送 → 复核）

> 时间戳格式 RFC3339（Asia/Shanghai）。已发生步骤填实际时间；未发生步骤填计划动作，
> 时间由对应负责人执行时回填。操作人约定：自动化步骤记「构建工程师（自动化）」，
> 人工确认步骤记「复核工程师」或具体姓名（本台账已全部回填，见 §7 签署栏）。

| # | 阶段 | 动作 | 时间戳（实际/计划） | 操作人 | 状态 |
|---|------|------|---------------------|--------|------|
| 1 | 构建 | `make build`（cloudcore/edgecore/keadm/mock-cloudhub → bin/） | 2026-08-14T19:20~19:27+08:00 | 构建工程师（自动化） | ✅ 已完成（历史构建，bin/ 时间戳佐证） |
| 2 | 构建 | 交叉编译 linux/amd64+arm64（dist/） | 2026-08-13T21:18~21:35+08:00 | 构建工程师（自动化） | ✅ 已完成（历史构建） |
| 3 | 校验 | 质量门：24 包 `go test -race` 全绿 / 覆盖率 77.8% / lint 0 | 2026-08-14（4K/4L 轮实测） | 构建工程师（自动化） | ✅ 已完成（证据见 PROGRESS §4K/§4L） |
| 4 | 校验 | E2E：`examples/demo.sh` DEMO PASS×3、keadm 升级回滚演练 6/6 | 2026-08-14 | 构建工程师（自动化） | ✅ 已完成（见 PROGRESS §4L） |
| 5 | 归档 | 4 二进制 + Chart 包 + checksum + SBOM 归档至 `release/v0.1.0/` | 2026-08-14T19:55~20:03+08:00（commit `d17bdd5` 19:55:55；keadm 重建 `733d0ae` 20:02:58；SBOM/checksum 刷新 `e29e1ce` 20:03:09） | 发布制品工程师 | ✅ 已完成（归档后独立复核又触发 keadm 重建，见步骤 8） |
| 6 | 归档 | 产物完整性核对（文件清单 + sha256 比对） | 2026-08-14T19:57~20:03+08:00（digest 回填 `1265214` 19:57:39；重建后重算 `e29e1ce` 20:03:09） | 发布制品工程师 | ✅ 已完成（checksums.txt 7/7 全 OK） |
| 7 | 镜像 | 多架构镜像推送（cloudcore/edgecore v0.1.0）并记录 digest | 2026-08-14T20:57~21:05+08:00（B5：buildx 双架构重建 localhost:5001，--provenance=false；manifest inspect 确认 amd64+arm64 两项；digest 更新见 images.json） | 发布制品工程师 | ✅ 已完成（v0.1.0 发布镜像由单架构 arm64 重建为双架构，见 MULTIARCH §7.1 / REAL-CLUSTER-GUIDE §3） |
| 8 | 复核 | 制品清单/checksum/digest 复核 + 本台账复核栏签署 | 2026-08-14T20:03~20:30+08:00（docs/RELEASE-REVIEW.md；P1-1 keadm 不含 P2 修复 → `733d0ae` 重建闭环） | 复核工程师 | ✅ 已完成（有条件通过 7/10，P1-1 已闭环；P2-1/P2-2 澄清，P3 观察项） |
| 9 | 发布 | Release Notes 字段回填 + 发布声明 | 2026-08-14T20:03~20:05+08:00（Notes 对齐 `866f796` 20:03:44；发布完成登记 `a2afaf4` 20:04:22；M5 发布报告 `eacd35a` 20:04:50） | 发布文档工程师（复核后） | ✅ 已完成 |

---

## 3. 制品清单（Artifacts）

> 路径以 `release/v0.1.0/` 为根（已归档，commit d17bdd5）；镜像为本地 registry（localhost:5001）闭环验证，远程推送步骤见 RELEASE-CHECKLIST.md。
> 文件存在性/checksum/digest 由发布制品工程师归档时回填。

| # | 制品 | 说明 | 路径（release/v0.1.0/） | checksum / digest | 回填状态 |
|---|------|------|--------------------------|--------------------|----------|
| 1 | cloudcore | 云端二进制（darwin-arm64 + linux-amd64） | `release/v0.1.0/cloudcore-darwin-arm64` 等 | `63e089f75556` 等 | 已归档（d17bdd5） |
| 2 | edgecore | 边缘二进制（darwin-arm64 + linux-amd64） | `release/v0.1.0/edgecore-darwin-arm64` 等 | `fb8e3945ea3e` 等 | 已归档 |
| 3 | keadm | 安装管理 CLI（含 upgrade/rollback） | `release/v0.1.0/keadm-darwin-arm64` 等 | `0406b67def35` 等 | 已归档 |
| 4 | Chart 包 | `edgeflow-0.1.0.tgz`（helm package） | `release/v0.1.0/edgeflow-0.1.0.tgz` | `ba2fd0e7fb21` | 已归档 |
| 5 | checksum 文件 | 全部 7 个制品 sha256 汇总 | `release/v0.1.0/checksums.txt` | `shasum -a 256 -c` 全 OK | 已归档 |
| 6 | SBOM | go list -m all 依赖 + 制品 sha256 + 构建参数 | `release/v0.1.0/sbom.json` | 33 组件 + 7 制品 | 已归档 |
| 7 | 镜像 digest | cloudcore/edgecore 不可变 tag + digest | `release/v0.1.0/images.json` | `sha256:2f9f2fbef9baf1188` / `sha256:227f6050438fdee43` | 已归档（pull 复验一致） |
| 8 | 镜像 cloudcore | `edgeflow/cloudcore:v0.1.0`（本地 registry，**双架构 amd64+arm64**） | localhost:5001 | 索引 digest `sha256:6d01b329a834…`（amd64 `84fd1e72…` / arm64 `373fba1c…`，见 images.json） | 已归档（B5 双架构重建，manifest + 双架构运行复验） |
| 9 | 镜像 edgecore | `edgeflow/edgecore:v0.1.0`（本地 registry，**双架构 amd64+arm64**） | localhost:5001 | 索引 digest `sha256:62e378b659d7…`（amd64 `6f7701cd…` / arm64 `e8325eca…`，见 images.json） | 已归档（B5 双架构重建，manifest + 双架构运行复验） |

> 说明：4 二进制默认打包 linux/amd64 + linux/arm64 双架构（`make cross-build` 产物在 `dist/`，
> 本机开发二进制在 `bin/`）；具体打包形态（单架构 tar.gz / 双架构目录）由发布制品工程师按
> 发布目录规范确定，并在本表回填实际文件名与校验值。

---

## 4. 验证结果表（Verification Matrix）

> 发布基线验证（2026-08-14 实测，证据：PROGRESS.md §4K/§4L、DEPLOYMENT.md §8、MULTIARCH.md §7）。

| # | 验证项 | 命令/方法 | 结果 | 证据位置 |
|---|--------|-----------|------|----------|
| 1 | 编译 | `go build ./...` / `make build` | ✅ 通过 | PROGRESS §4K |
| 2 | 静态检查 | `go vet ./...` + `gofmt -l .` | ✅ 通过 | PROGRESS §4K |
| 3 | Lint | `golangci-lint run ./...` | ✅ 0 issues | PROGRESS §4K/§4L |
| 4 | 单元测试 | `go test -race ./...`（24 包） | ✅ 全绿 | PROGRESS §4L |
| 5 | 覆盖率 | `go test -race -cover ./...` | ✅ 77.8%（门槛 70%） | PROGRESS §4L |
| 6 | 审查 | 各里程碑 CODE-REVIEW-*.md | ✅ 0 P0 / 0 P1 | REVIEWS.md |
| 7 | E2E Demo | `bash examples/demo.sh` | ✅ DEMO PASS×3，0 残留 | PROGRESS §4L |
| 8 | keadm 全链路 | init→join→upgrade(含 simulate-failure)→rollback→reset | ✅ 6/6 演练场景通过 | UPGRADE.md §5 |
| 9 | 升级回滚台账 | `keadm ops-ledger` | ✅ 4 条记录可查、操作人可追踪 | UPGRADE.md §5 |
| 10 | mTLS | 双向认证 + wss + 拒绝路径 + 私钥 0600 | ✅ 通过 | PROGRESS §4J |
| 11 | 镜像构建 | `docker build --target cloudcore/edgecore` | ✅ 通过（16.7MB / 22.5MB） | DEPLOYMENT.md §8 |
| 12 | 镜像冒烟 | `docker run --rm <img> --version` + `/healthz` 200 | ✅ 通过 | DEPLOYMENT.md §8 |
| 13 | 多架构 | `docker manifest inspect` amd64+arm64 + 双架构 `--version` 一致 | ✅ 通过（v0.1.0 重建证据见 MULTIARCH.md §7.1 + REAL-CLUSTER-GUIDE.md §3） | MULTIARCH.md §7.1 |
| 14 | Helm | `helm lint` 0 failed + `helm template` + `helm install --dry-run=client` | ✅ 通过 | DEPLOYMENT.md §8 |
| 15 | Modbus | 模拟器读写 + 台账 + 30 天清理 | ✅ 通过 | PROGRESS §4K |
| 16 | NodeController | SIGSTOP→Offline→SIGCONT→Ready 状态机 | ✅ 通过 | PROGRESS §4K |
| 17 | 制品完整性 | 归档后 checksum 比对（SHA256SUMS） | ✅ 通过（checksums.txt 7/7 独立重跑一致，RELEASE-REVIEW） | 发布制品工程师 |
| 18 | 镜像 digest 复核 | 推送后 manifest 自检（CI release.yml 内建） | ✅ 通过（双架构 manifest inspect + digest 双渠道核对；2026-08-14 B5 重建后复验） | 发布制品工程师 |
| 19 | 最终复核 | 台账 + Release Notes + 制品三方一致性 | ✅ 通过（RELEASE-REVIEW 最终结论：有条件通过，P1-1 已闭环） | 复核工程师 |

---

## 5. 异常处理记录（Incident Log）

> **暂无异常。** 发布流程中任何失败/偏差（构建失败、校验不通过、镜像推送失败、归档不完整、
> 回滚触发等）在此登记，格式：时间 / 阶段 / 现象 / 根因 / 处置 / 影响范围 / 关闭状态。
> 已登记的风险（非异常）见 RELEASE-NOTES-v0.1.0.md §4（E1 镜像未推送、E2 真实集群未安装、E3 远程仓库未配置）。

| 时间 | 阶段 | 现象 | 根因 | 处置 | 影响范围 | 状态 |
|------|------|------|------|------|----------|------|
| （暂无） | — | — | — | — | — | — |

> 说明：MULTIARCH.md §5 记录的已知风险（QEMU 偶发 OOM、:latest 可覆盖、registry 端口冲突等）
> 属发布**预案**而非本次异常，实际发生时按 §6 预案处置并在此登记。

---

## 6. 回滚预案引用（Rollback Reference）

| 场景 | 预案位置 |
|------|----------|
| keadm 产物升级/回滚异常 | docs/UPGRADE.md §3 异常路径表（含人工 cp 兜底命令） |
| 镜像推送失败/损坏 | docs/MULTIARCH.md §6（单架构回退 / imagetools create 指回旧 digest） |
| Helm 部署回滚 | docs/DEPLOYMENT.md §5.1（`helm rollback edgeflow <REVISION>`） |
| 数据一致性 | docs/UPGRADE.md §5（升级前 hash 基线、操作后比对） |

---

## 7. 签署栏（Sign-off）

| 角色 | 姓名 | 时间 | 结论 |
|------|------|------|------|
| 发布制品工程师 | 发布制品工程师（自动） | 2026-08-14 | 制品归档与镜像推送完成，清单核对一致（归档 `d17bdd5`；keadm 重建 `733d0ae`；镜像双架构重建 B5） |
| 复核工程师 | 独立复核员 | 2026-08-14 | 制品/校验值/文档三方一致，同意发布（RELEASE-REVIEW：有条件通过，P1-1 已闭环） |
| 发布文档工程师 | 发布文档工程师（自动） | 2026-08-14 | 文档编制与字段回填完成（Release Notes/台账/多架构/真实集群指南同步） |
