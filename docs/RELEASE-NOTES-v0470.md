# EdgeFlow v0.47.0 发布说明（应用目录 + 应用级灰度 + 离线安装包 + 脚本化应用）

- 发布日期：2026-10-13
- 基线：v0.46.0（ae0d1cd）
- 主题：应用目录与分发增强（G08/G09/G10/G07，spec 0020）。应用目录/版本目录
  （container|script 双形态）、应用级灰度分批/暂停/恢复/取消/回滚（独立状态机）、
  离线安装包缓存（云端上传/下载 sha256 + 边缘包缓存）、脚本化应用生命周期
  （opt-in scriptrunner：sha256 硬校验 + 白名单生命周期执行 + PodStatus 形态
  回报）。契约扩容 82→97 端点；消息 16 维持（PodSync/ConfigSync 复用，
  零新消息类型）；默认零行为（无应用/无计划/scriptrunner off 时零变化）。

## N1 能力（新增）

### 1. 应用目录与版本目录（G08，US-1）
- cloud/pkg/appstore：内存 map + JSON 穿写持久化
  （`EDGEFLOW_CLOUDCORE_APPSTORE_FILE`，未配置 = 纯内存，重启丢失为明示行为）。
- 应用三要素：name（DNS-1123 校验）+ kind（container|script）+ 版本目录
  （版本号唯一；container 版本必填 image、script 版本必填 scriptRef，互斥校验）。
- 端点 6：`POST /api/v1/applications`（409 重复）、`GET`（分页 limit/offset +
  X-Total-Count）、`GET/DELETE /{name}`（存在进行中部署 → 409）、
  `POST/GET /{name}/versions`（409 版本重复 / 400 校验失败）。

### 2. 应用级灰度部署（G09，US-2）
- cloud/pkg/appdeploy：独立状态机 pending→running→paused→
  succeeded/failed/cancelled（与 modelrelease 语义对齐、非代码复用，spec §3
  边界 ①）；批次 = 最小 batch 号 pending 节点集，BatchSize 分批 +
  pauseBetweenSec 批间停顿窗。
- 扫描拾起语义：控制器 Tick 拾起 pending 新计划（→ running）并推一批；
  paused 不被拾起（恢复必须显式 Resume）；批次收尾即汇总终态（最后一批交付的
  同一扫描周期内到达终态，全失败 → failed 否则 succeeded——尽力交付语义，
  部分节点失败不回滚其余节点）。
- 端点 7：`POST /api/v1/app-deployments`（201 + 计划 ID）、`GET`（列表分页）、
  `GET /{planID}`（perNode 状态/Reason 留痕）、`POST /{planID}/pause|resume`
  （状态冲突 409）、`/cancel`（逆序卸载已交付节点）、`/rollback`（body 可选
  {version}，缺省取目录中另一版本；对原 delivered 节点集再下发目标版本——
  PodSync update 覆盖同名 Pod 即等效回滚）。
- 交付执行器（Deployer 接口注入）：container → PodSync add/update
  （pod name = `edgeflow-app-<app>`，upsert 幂等）；script → ConfigSync
  （见 §4）。Deployer 从 Cancel/卸载 goroutine 调用——**实现须并发安全**
  （契约注记；fake 未加锁曾在 -race 下暴露）。

### 3. 离线安装包缓存（G10，US-3）
- 云端：`PUT /api/v1/applications/{name}/versions/{version}/package`（≤32MB
  硬上限：Content-Length 预检 + MaxBytesReader 双保险；落盘
  `EDGEFLOW_CLOUDCORE_APPPKG_DIR`，0600 原子写）→ MarkPkg 登记 + sha256 回显；
  `GET .../package`（X-Checksum-Sha256 + attachment）。
- 边缘：scriptrunner 包缓存目录 `EDGEFLOW_EDGED_APPPKG_DIR`（<dir>/<app>/
  <version>.tgz）——部署成功即缓存，为离线重启用留底（离线部署为预下发形态，
  spec §3 边界 ②）。

### 4. 脚本化应用生命周期（G07，US-4）
- 下发形态：ConfigSync ConfigMap data 内联
  `{app, version, scriptRef, sha256, scriptTar(base64 tar.gz)}`——契约零新增
  消息；内联上限 8MB（base64 后 ~10.7MB），超出拒绝交付并留痕（大包走离线
  预下发面）。
- 边缘 scriptrunner（edge/pkg/edged，opt-in `EDGEFLOW_EDGED_SCRIPTRUNNER=on`，
  **默认 off 零行为**——脚本配置仅落盘 + Warn）：
  - sha256 硬校验（云端登记 ↔ 边缘落地一致，不一致拒绝执行、不解包、记
    failed）；
  - 包落缓存 → 解包专用工作目录（<dir>/work/<app>-<version>/，防路径穿越：
    拒绝 `..`/绝对路径成员）；
  - 生命周期白名单 install/start/stop/uninstall（包根同名文件，`sh -e` 执行，
    60s 超时、输出截断 2KB、动作/退出码/耗时/输出摘要审计日志；缺失动作跳过）；
  - 状态回报：PodStatus 形态复用（pod.name = `edgeflow-script-<app>`，
    running→Running / failed→Error / stopped→Stopped），伪 Pod 经既有上报
    循环回穿（extras 注入，上报周期配置复用）——契约零新增消息；
  - delete 卸载：stop → uninstall → 工作目录清理 → 状态清空（尽力，失败记
    日志继续清理）。
- 装配：cmd/edgecore ConfigSync 分发钩子（MetaManager 落盘成功后执行，执行
  失败不回错——配置落盘与脚本执行解耦，执行态经 PodStatus 暴露）。

## N2 兼容与冻结
- **契约扩容**：82→97 端点（applications 6 + package 2 + app-deployments 7）；
  消息 16 维持（PodSync upsert 语义、ConfigSync 脚本内联 data 键 = 只增不改，
  旧边缘按既有 ConfigMap 落盘语义处理）。
- **默认零行为**：无应用注册/无部署计划 → appstore/appdeploy 空转零副作用；
  scriptrunner 默认 off（脚本配置仅落盘）；未配置 APPSTORE_FILE/APPKG_DIR 时
  应用面端点 503 明示未启用（契约探测分支覆盖）。
- 冻结带 v0240–v0350 测试零改动；v0.39–v0.46 测试零改动；go.mod 零变化
  （tar/gzip/sha256 全标准库）。

## N3 测试（grep 口径）
- cloud/pkg/appstore +2（CRUD/校验/版本唯一/持久化重载/ErrExists·ErrNotFound）。
- cloud/pkg/appdeploy +3（状态机分批推进/终态汇总/rollback delivered 集合/
  cancel 逆序卸载——fakeDeployer 并发安全化后 -race 绿）。
- edge/pkg/edged +4（默认关闭零行为/生命周期真实执行（真实 sh 进程）/sha256
  不一致拒绝执行不解包/非脚本配置零行为）。
- tests/e2e +1（TestV0470AppCatalogDeployE2E：US-1 目录注册/409/版本校验/
  分页 + US-3 包上传下载 checksum + US-2 双节点分批交付/离线尽力/回滚计划 +
  US-4 脚本应用内联下发/sha256 校验/install+start 执行/伪 Pod 回穿全链）。
- 合计：新增 10 例（单测 9 + e2e 1）全绿。

## N4 边界（登记 KNOWN-ISSUES §48）
- 灰度状态机独立实现（与 modelrelease 语义对齐非代码复用）；Tick 扫描周期即
  批间节奏（pauseBetweenSec 为批后附加窗）；回滚只恢复目标版本（不跨多版链）；
  部分失败尽力语义（不自动回滚成功节点）；appstore 未配文件重启丢失；
  脚本包内联 ≤8MB；部署目标节点须已注册（未注册按交付失败留痕）；伪 Pod 仅
  生命周期态（无资源占用指标）；scriptrunner 白名单最小化（非通用 shell 托管）。

## N5 门禁（2026-10-13）
- [1] `go vet ./...`：VET OK；gofmt 清洁。
- [2] `go test -race`（触碰 5 包：cloud/pkg/appstore / appdeploy /
  edge/pkg/edged / cmd/cloudcore / cmd/edgecore）：全绿。
- [3] 契约 `go test ./tests/contract/`：**16.527s** 全绿（97 端点/16 消息；应用面 503 未
  启用探测分支）。
- [4] e2e 全量 `-timeout 25m`：**686.573s exit=0**（完整日志
  .cluster/edgeflow-v0470/e2e-full-v0470.log）。
- [5] e2e 期间修复三处开发缺陷（诚实登记）：
  ① pending 计划无人启动（Tick 只扫 running，Create 后永挂 pending）→ 扫描
  拾起语义；② 回滚计划未终态即删应用 409（e2e 等待缺失，行为本身正确）；
  ③ 伪 Pod 名 `edgeflow-script-<app>` 双段拼接 e2e 断言错位。

## N6 复核（15min 硬时限，实际 7m56s）
- 原始结论：**FAIL（P1×3 / P2×3 / P3×7）**；处置后全绿放行（回执
  .cluster/edgeflow-v0470/review.md，原始报告 .cluster-review-v0470.md）。
- **P1×3 全修**：①sha256 fail-open → fail-closed（缺 checksum 拒绝执行，
  新增用例断言不解包）；②work 目录穿越（deploy/uninstall 补 Base 收敛，与
  缓存路径同口径）；③advance 终态写点绕过断言（Cancel 竞态）→ 写点前
  Terminal 复查 + Cancel 快照化 + 终态竞态补偿卸载本批 delivered。
- **P2×3 全修**：回滚缺省目标改「严格早于当前版本的最新版」（spec 边界⑤）；
  HandleConfigSync 异步化（单 worker 串行队列，EdgeHub 读循环不被 ~120s
  生命周期执行阻塞）；脚本执行最小 env（不继承凭证类变量）。
- **P3**：顺手修 3（StatusPayloads 注释对齐 Error 相位/DELETE EqualFold/
  rollbackNodes 死参数）；留档 4（创建 200 vs 版本 201 契约已定稿/DELETE
  持久化失败误报 404/truncate UTF-8/解包总量与孙进程治理）→ KI §48。
- 自查修正（复核派出前）：应用版本字符集收紧（仅字母/数字/点/下划线/连字符
  且拒双点序列）+ appPkgPath 双重 Base——路径穿越面纵深防御。
- 修正后复验：race（appdeploy/appstore/edged）绿 + 契约 13.270s 绿 +
  定向 e2e 21.472s PASS（异步 worker 语义下全链）。
