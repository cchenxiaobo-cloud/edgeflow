# EdgeFlow M5 v0.1.0 发布独立复核报告（RELEASE REVIEW）

> 评审人：独立复核员（非构建人）
> 评审日期：2026-08-14
> 评审对象：release/v0.1.0/ 发布制品 + 配套发布文档
> 依据：docs/RELEASE-CHECKLIST.md（含各条"如何验证"）
> 范围边界：本机无远程凭据，本地 registry 闭环；演练排期为建议项（需运维确认窗口）

## 初版结论（骨架建立时）

- [ ] 制品完整性
- [ ] 校验和
- [ ] SBOM
- [ ] Chart 包
- [ ] 镜像 digest
- [ ] Release Notes
- [ ] 发布台账
- [ ] 演练排期
- [ ] 回滚方案
- [ ] P2 闭环

**最终结论：⚠️ 有条件通过（Conditional Pass）** —— 7/10 通过、2 项有条件通过、1 项不一致需处置（P1-1：keadm 二进制不含 P2-4/P2-5 修复）。详见文末问题清单。

---

## 逐项复核记录

（按序逐项填写：验证方法 / 实际结果 / 通过否）

---
### 1. 制品完整性（release/v0.1.0/）

- 验证方法：`ls -la` 核对 7 个文件；`-x` 检查 6 二进制可执行位；版本抽查 3 个（darwin 直跑 cloudcore/keadm + linux-amd64 经 alpine:3.20 容器跑 cloudcore）
- 实际结果：
  - 7/7 文件存在：6 二进制（cloudcore/edgecore/keadm × darwin-arm64 + linux-amd64）+ edgeflow-0.1.0.tgz
  - 6/6 二进制可执行（-rwxr-xr-x）
  - `./cloudcore-darwin-arm64 --version` → `version=v0.1.0 gitCommit=ca2051b buildTime=2026-08-14T19:52:32+0800 goVersion=go1.26.2` ✅
  - `./keadm-darwin-arm64 version` → `keadm version=v0.1.0 gitCommit=ca2051b buildTime=2026-08-14T19:52:32+0800 goVersion=go1.26.2` ✅
  - docker alpine 容器跑 `cloudcore-linux-amd64 --version` → `version=v0.1.0 gitCommit=ca2051b ...`（与 darwin 产物同 commit/同 buildTime）✅
- 通过：✅ 通过

### 2. 校验和（checksums.txt 独立重跑）

- 验证方法：`wc -l` 核对条目数；`cd release/v0.1.0 && shasum -a 256 -c checksums.txt` 独立重跑
- 实际结果：checksums.txt 共 7 行，覆盖 6 二进制 + 1 Chart 包；`shasum -a 256 -c` 独立重跑 7/7 全 `OK`
- 通过：✅ 通过
### 3. SBOM（sbom.json）

- 验证方法：`python3 -m json.tool` 合法性；组件数与 go.mod 依赖比对（`go list -m all`）；制品 sha256 与 checksums.txt 交叉核对（抽查 2 个，实际全 7 个）
- 实际结果：
  - JSON 合法 ✅；SPDX 结构完整（bomFormat/specVersion/serialNumber/metadata/components/artifacts）
  - components = 33（≥ 20 ✅）；`go list -m all` 排除主模块后 33 个模块，与 SBOM 组件逐条比对 **完全一致**（双向无差异）
  - Go 版本与构建参数在 metadata 中：tools=go1.26.2；properties 含 gitCommit=ca2051b / buildTime / ldflags ✅
  - artifacts 7 条（6 二进制 + 1 chart），sha256 与 checksums.txt **7/7 全部一致**（超过抽查 2 个的要求）
- 通过：✅ 通过

### 4. Chart 包（edgeflow-0.1.0.tgz）

- 验证方法：`helm show chart` 核对 version/appVersion；`tar tzf` 抽查包内 templates；values.yaml 与源码 build/charts/edgeflow/values.yaml 比对
- 实际结果：
  - `helm show chart` → version=0.1.0、appVersion=v0.1.0、name=edgeflow、type=application ✅
  - `tar tzf` 包内文件齐全：Chart.yaml / values.yaml / templates/(NOTES.txt, _helpers.tpl, cloudcore-deployment.yaml, cloudcore-service.yaml) / .helmignore ✅
  - values.yaml 与 build/charts/edgeflow/values.yaml **逐字节一致** ✅
  - Chart.yaml 与源码存在文本差异（注释被剥离、字段重排、引号归一化），经 key-value 解析比对：name/version/appVersion/type/description 全部一致 → 属 helm package 归一化，语义无差异 ⚠️（记录为观察项，非问题）
  - `helm lint` 0 failed；`helm template --dry-run` 渲染成功 ✅
- 通过：✅ 通过（附观察项 1 条）

### 5. 镜像 digest（images.json）

- 验证方法：jq/python 核对结构（tag/digest/size/arch/buildArgs）；gitCommit=ca2051b 与 git log 对应；registry 已清理（无凭据，无法 curl API 复验，以本地记录 + 构建人声明为准）
- 实际结果：
  - 结构完整 ✅：version/registry/generatedAt/notes/images[2]，每镜像含 name/tag/reference/digest/size/arch/os/buildArgs 全字段
  - buildArgs 与发布二进制一致 ✅：GIT_COMMIT=ca2051b、BUILD_TIME=2026-08-14T19:52:32+0800、VERSION=v0.1.0（与 --version 输出逐字段吻合）
  - gitCommit=ca2051b 与 git log 对应 ✅（`git log --oneline` 确认 ca2051b 为真实提交）
  - digest 独立复验：**无法执行**（本地 registry 已清理，属环境边界）→ 依赖 images.json notes（registry API Docker-Content-Digest + docker inspect RepoDigests 双渠道一致）+ 构建人 pull 复验声明
  - ⚠️ arch 字段标注 `linux/arm64` 单平台，而 Release Notes §7 声明 amd64+arm64 单 tag 双平台 manifest；且 images.json digest（2f9f2fbe/227f6050）与 MULTIARCH.md §7 记录的本地验证 digest（cloudcore amd64 307c75fa…/arm64 81df9cdb…）不一致 —— 经查 MULTIARCH §7 的镜像运行证据对应**旧构建**（gitCommit=f71684e、buildTime=unknown、go1.26.6），新 digest 无对应 manifest inspect / --version 运行证据留存（仅构建人声明）
- 通过：⚠️ 有条件通过（结构/参数一致 ✅；新 digest 无法独立复验 + arch 标注歧义 + 文档证据链指向旧构建 → 需构建人澄清，见问题清单 P2-1/P3-1）

### 6. Release Notes（RELEASE-NOTES-v0.1.0.md）

- 验证方法：grep 章节结构；已知问题与 PROGRESS.md 交叉抽查 3 条
- 实际结果：
  - 中英双语 ✅：§0 English Summary + 全中文正文
  - 四节齐全 ✅：§2 变更内容 / §4 已知问题 / §5 升级步骤 / §6 回滚说明
  - 已知问题交叉抽查：
    - K1（restoreBackup 非事务性）↔ PROGRESS §4L P2-4 已修复：Notes 标"本轮闭环中"，方向一致 ✅（但见问题清单 P1-1：修复未进发布二进制）
    - K2（manifest 白名单）↔ PROGRESS §4L P2-5 已修复：同上 ✅
    - K17（cmd-edgecore 56.5% 覆盖率缺口）↔ PROGRESS §5 Backlog：数字一致 ✅
    - 观察：K3（MQTT broker 生命周期）来源为 CODE-REVIEW-M3B.md，§5 无直接对应条目；§5 Backlog 存在已完成未清理条目（M2 完整化 / M3 MQTT EventBus / Helm Chart 骨架），属文档维护问题
  - ⚠️ §1 发布信息仍标【待回填】：发布日期/发布制品 commit/制品位置，且文件头状态仍为"发布制品制作中"——与台账已回填（commit 1265214）不同步（发布日期未定与 E1/E3 未闭环自洽，但"制品位置/制品 commit 待回填"已过时）
- 通过：✅ 通过（附观察项 2 条）

### 7. 发布台账（RELEASE-LEDGER.md）

- 验证方法：核对五要素（时间线/操作人/制品清单/验证结果/异常处理）
- 实际结果：
  - 时间线 §2 ✅ 存在（9 步：构建→校验→归档→镜像→复核→发布）
  - 操作人 ✅（构建工程师/发布制品工程师/复核工程师/发布文档工程师角色清晰）
  - 制品清单 §3 ✅ **已回填实际值**（commit 1265214：7 个本地制品 checksum/digest 真实值 + "已归档（d17bdd5）"状态；行 8-9 远端镜像 digest 标【回填】+ 本地验证参考值）
  - 验证结果 §4 ✅ 19 行矩阵（含证据位置）
  - 异常处理 §5 ✅（"暂无异常" + 格式说明）
  - ⚠️ 观察：§2 时间线第 5-9 步（归档/镜像/复核/发布）仍标 ⏳ 待执行 + 【回填】，与 §3 已归档、images.json 已出、pull 复验已完成的实际状态不一致；§7 签署栏复核工程师待签（预期由本复核完成）
- 通过：✅ 通过（附观察项 1 条）

### 8. 演练排期（DRILL-SCHEDULE.md）

- 验证方法：核对六要素
- 实际结果：时间窗口（§1，明确标注【需确认】+ 观察期 7 天建议）✅ / 范围（§2 场景链 9 步 + 判定总则）✅ / 参与人（§3 角色表，操作与复核分离，标注【需确认】）✅ / 前置条件（§4 六项 checklist）✅ / 回滚预案（§5 P0-P2 优先级止血表）✅ / 通知模板（§6 申请邮件+群预告+结束通知）✅；另有 §7 归档、§8 行动项（责任方/截止/状态）✅
- 通过：✅ 通过（标注需确认，符合环境边界）

### 9. 回滚方案（Release Notes §6 ↔ UPGRADE.md 一致性）

- 验证方法：逐条比对 RELEASE-NOTES §6 回滚表与 UPGRADE.md §3 异常路径表/§2 操作/§5 演练
- 实际结果：
  - keadm 产物回滚：`keadm rollback --latest/--backup=<id>` ↔ UPGRADE §2.2 ✅；失败兜底（备份保留+人工 cp 路径）↔ §3 行 2 ✅
  - 无可用备份 → 重新 join ↔ §3 行 5 ✅
  - Helm 云端回滚 `helm rollback edgeflow <REVISION>` ↔ DEPLOYMENT §5.1 引用一致 ✅
  - 镜像回退（单架构 tag 覆盖 / imagetools create 指回旧 digest）↔ MULTIARCH §6 ✅
  - 数据兼容：keadm 不触碰数据目录 + SQLite 跨小版本兼容 + 升级前快照 ↔ UPGRADE §3 行 3 / §5 ✅；"回滚后边缘重启自动重新注册"与 M2 自治设计一致 ✅
  - 观察：UPGRADE.md §4 仍写"真实节点回滚…留待 M5"——本发布即为 M5，措辞过时（机制与 M5 发布流程实际衔接无冲突）
- 通过：✅ 通过（附观察项 1 条）

### 10. P2 闭环（M4D P2×2）

- 验证方法：PROGRESS.md 查 M4D P2-4/P2-5 状态与 commit；git show 核对修复内容与时间线
- 实际结果：
  - 文档状态 ✅：PROGRESS.md §4L "M4D P2×2 处置（2026-08-14，commit fe093e1）"：P2-4 restoreBackup 事务化（staging+原子替换）、P2-5 备份清单白名单校验，均标"已修复"；ef21a98 为台账更新 commit（文档闭环完整）
  - **⚠️ 严重不一致（问题 P1-1）**：修复提交 fe093e1 = 2026-08-14 19:55:23，修改 cmd/keadm/rollback.go（+93/-14）；而发布二进制 buildTime=19:52:32、gitCommit=ca2051b —— **v0.1.0 keadm 二进制构建早于修复提交，不包含 P2-4/P2-5 修复**。即：PROGRESS/Release Notes 宣称"已修复/本轮闭环"，但已发布 keadm 的 rollback 仍为非事务性 + 无白名单校验（git 可复现性：ca2051b 检出即无修复）
- 通过：❌ 文档闭环 ✅，但**发布制品未包含所宣称修复** → 见问题清单 P1-1

---

## 总结论

**⚠️ 有条件通过（Conditional Pass）**

10 项复核中：7 项通过、2 项有条件通过（镜像 digest、P2 闭环）、1 项含阻断性不一致需处置（P2 闭环）。制品整体可用（本地闭环范围内），但存在 1 项**发布内容与文档声明不一致**需在对外发布前处置，另有若干低危观察项。

## 问题清单（按严重度）

### P1（发布前须处置/澄清）

- **P1-1 已发布 keadm 二进制不含 P2-4/P2-5 修复，与 PROGRESS/Release Notes 声明不一致**
  - 证据：二进制 `gitCommit=ca2051b`、`buildTime=2026-08-14T19:52:32+0800`；修复提交 `fe093e1`（19:55:23，改动 cmd/keadm/rollback.go +93/-14）晚于构建时间；git log 时间线完整佐证
  - 影响：v0.1.0 keadm 的 rollback 不具备事务化 restore 与 manifest 白名单校验；PROGRESS §4L"已修复"与 Release Notes K1/K2"本轮闭环中"对发布制品而言不成立
  - 建议（二选一）：① 以 HEAD（含 fe093e1）重建 keadm 两平台二进制，同步重算 checksums.txt/sbom.json 并更新台账/Notes；② 若维持现状，在 Release Notes K1/K2 与台账中明确"修复未包含于 v0.1.0，随 v0.1.1"，并将 §5 Backlog 对应条目恢复为未闭环
  - 严重度：中（P2 级缺陷，不影响 v0.1.0 核心功能与既有兜底路径；主要问题是文档与制品不一致）

### P2（建议补充，不阻断）

- **P2-1 镜像 digest 独立证据缺失 + arch 标注歧义**：registry 已清理无法复验（环境边界可接受），但文档中唯一镜像运行证据（MULTIARCH §7）对应旧构建 f71684e（buildTime=unknown/go1.26.6），与 images.json 新 digest（2f9f2fbe/227f6050，buildArgs ca2051b）无关联证据；且 arch 字段标 linux/arm64 与"双平台 manifest"声明不符。建议：构建人补充新 digest 的 manifest inspect 输出与 `--version` 运行记录，并明确 digest 类型（manifest list vs 单平台）；如有条件恢复 registry 复核
- **P2-2 台账 §2 时间线未回填**：归档/镜像/复核步骤仍标 ⏳ 待执行 + 【回填】，与 §3 已归档状态矛盾；建议回填实际时间并完成 §7 签署

### P3（观察项，文档维护）

- P3-1 images.json `generatedAt=19:58:00` 晚于其归档 commit 时间（d17bdd5=19:55:55），时间戳前向偏差，建议核实生成逻辑
- P3-2 Release Notes §1 发布信息（发布日期/制品 commit/制品位置）仍标【待回填】，与台账回填状态不同步；文件头状态行已过时
- P3-3 PROGRESS.md §5 Backlog 存在已完成未清理条目（M2 完整化、M3 MQTT EventBus、Helm Chart 骨架）；K3 等已知问题条目在 §5 无直接对应，来源为各审查文档
- P3-4 UPGRADE.md §4"真实节点回滚…留待 M5"措辞过时（本发布即 M5）
- P3-5 台账 §3 行 8-9 将旧构建 digest（MULTIARCH §7）标注为"本地验证参考"未注明构建差异，易混淆

## 复核签署

- 复核人：独立复核员（本报告）
- 复核日期：2026-08-14
- 依据：实际命令输出（--version 抽查、shasum -c、SBOM 解析比对、helm/tar、git log 时间线）；未能独立复验项已如实标注（registry 已清理、无远程凭据）
