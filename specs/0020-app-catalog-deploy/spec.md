# EdgeFlow v0.47.0 规格（spec 0020）——应用目录与分发增强

版本：v0.47.0（发展规划 G08 + G09 + G10 + G07）。状态：设计定稿（as-built 回填待实现后）。
基线：v0.46.0（ae0d1cd）；契约基线 82 端点 / 16 消息。

## 1. 背景与目标

应用分发差距评估（方案 §2.5）：A1 统一分发缺脚本/主机形态（G07）、A2 无应用目录
（G08）、A3 Pod 层无灰度（G09）、A4 无离线安装包缓存（G10）。现状：单 Pod 直接
下发（POST /nodes/{id}/podsync）、模型层灰度完整（v0.7–v0.23）、边缘容器运行时
（docker run 子集）。

本版目标（验收四支柱）：
1. **应用目录**：应用注册/版本目录/查询（G08）；
2. **应用级灰度**：分批下发/暂停/继续/回滚（G09，复用模型灰度状态机语义）；
3. **离线安装包**：应用包上传/缓存/断网部署（G10）；
4. **脚本化应用**：受控脚本生命周期（install/start/stop/uninstall，全程审计）（G07）。

## 2. 用户故事

### US-1 应用注册与版本目录（G08）
作为运营者，我在云端注册应用并维护版本目录：
- 应用对象：`{name, displayName, description, kind: container|script, versions: []}`；
- 版本对象：`{version, image?(container), scriptRef?(script), metadata{...}}`；
  kind 决定 image/scriptRef 二选一（校验 400）；
- API（6 端点）：
  - `POST /api/v1/applications`（注册；name 唯一 409）
  - `GET /api/v1/applications`（列表；分页 limit/offset + X-Total-Count 惯例）
  - `GET /api/v1/applications/{name}`（详情含版本目录）
  - `DELETE /api/v1/applications/{name}`（删除；有进行中部署 → 409）
  - `POST /api/v1/applications/{name}/versions`（登记版本；重复 409）
  - `GET /api/v1/applications/{name}/versions`（版本列表）
- 存储：进程内 store + 可选 JSON 文件持久化（EDGEFLOW_CLOUDCORE_APPSTORE_FILE，
  沿用 mediastore/appstore 写穿模式）。

### US-2 应用级灰度部署（G09）
作为运营者，我把某应用某版本灰度下发到节点集合：
- API（6 端点）：
  - `POST /api/v1/applications/{name}/deployments`（创建部署计划：
    `{version, nodes:[...], batchSize, pauseBetweenSec}` → planID，状态机
    pending→running→paused→succeeded/failed/cancelled——语义对齐模型灰度
    （modelrelease 状态机形态，实现独立于 modelrepo，零耦合））
  - `GET /api/v1/app-deployments`（全局列表）/ `GET /api/v1/app-deployments/{planID}`
    （详情：perNode 状态/批次进度）
  - `POST /api/v1/app-deployments/{planID}/pause` / `/resume` / `/cancel`
    （运行态控制；非法态 409）
  - `POST /api/v1/app-deployments/{planID}/rollback`（回滚到目标版本——
    按批次逆序恢复上一版本 Pod 规格）
- 下发通道：复用 PodSync（operation=add/update/delete + pod 规格），零新消息；
  脚本应用经 ConfigSync 下发脚本包引用（见 US-4）。
- 批次执行：控制器 goroutine（扫描 → 锁 → 批次推进），复用 modelrelease 的
  跑马灯形态（ScanInterval/LockKV/批内并发配置）——代码复用接口形态而非 import
  （modelrelease 与 appdeploy 各自独立包，共享 etcdstore.LockBackend 能力面）。

### US-3 离线安装包缓存（G10）
作为边缘运维者，弱网/断网场景下从本地缓存部署应用：
- 云端：应用包上传 `PUT /api/v1/applications/{name}/versions/{version}/package`
  （body: tar 字节流，≤32MB 校验；存储 EDGEFLOW_CLOUDCORE_APPPKG_DIR，落盘
  sha256 命名）；下载 `GET .../package`（边缘/运维取包；响应带 X-Checksum-Sha256）；
- 边缘：spool 缓存目录（EDGEFLOW_EDGED_APPPKG_DIR）——部署前尝试从云端拉包
  （有网）→ 校验 sha256 → 落缓存；离线时直接用缓存（缺失 → 部署失败带明确错误）；
- 断网语义：离线部署属「预下发」（运维手动触发/计划任务后续），不承诺自动
  离线感知；e2e 用「先拉包断网再部署」形态验证。

### US-4 脚本化应用生命周期（G07）
作为现场工程师，我部署受控脚本应用到边缘主机：
- 脚本应用 = 注册为 kind=script 的应用（scriptRef 指向脚本包）；下发经
  ConfigSync（ConfigMap data: script.tar.gz(base64) + sha256 + 生命周期声明）；
- 边缘执行器（edged scriptrunner，opt-in EDGEFLOW_EDGED_SCRIPTRUNNER=on，默认
  off 零行为）：脚本包落盘 → 生命周期动作（install/start/stop/uninstall）经
  受控白名单执行（sh -e 超时 60s、输出截断留痕、以专用工作目录执行）；
- 状态回报：PodStatus 形态复用（pod.name = edgeflow-script-<app>，state 映射
  脚本生命周期状态）——契约零新增消息；
- **安全硬规则**：脚本包必须 sha256 校验（云端登记 ↔ 边缘落地一致）；执行
  全程审计（动作/退出码/耗时/输出摘要进 ops 留痕）；非 opt-in 时 ConfigSync
  脚本配置仅落盘不执行（Warn 提示未启用）。

## 3. 兼容与边界
- 契约：+13 端点（applications 6 + app-deployments 5 + package 2），82→95；
  消息 16 维持（PodSync/ConfigSync 复用，零新消息类型）。
- 默认零行为：无应用注册/无部署计划时零行为；scriptrunner 默认 off。
- 冻结带 v0240–v0350 测试零改动；go.mod 零变化。
- 边界登记：①灰度状态机独立实现（与 modelrelease 语义对齐非代码复用）；
  ②离线部署为预下发形态（非自动离线感知）；③脚本执行器白名单最小化
  （非通用 shell 托管）；④应用包 ≤32MB；⑤回滚只恢复上一版本（不跨多版）。

## 4. 验收口径
- 应用目录：注册/版本/分页/409 语义 e2e；
- 灰度：2 节点 batchSize=1 → 逐批推进 → pause → rollback 全链 e2e；
- 离线：拉包 → 断网（不启动云端）→ 边缘缓存命中部署 e2e；
- 脚本：opt-in 执行 install/start → 状态回报 → uninstall 全链 e2e（审计留痕）。


---

## 4. 交付锚（v0.47.0，2026-10-13）

- 契约终态：**97 端点**（应用目录 6 + 应用包 2 + 灰度部署 7 + 既有 82）/
  消息 16 维持（PodSync upsert、ConfigSync 脚本内联 data 键 = 只增不改）。
- 测试锚：新增 10 例（appstore 2 / appdeploy 3 / scriptrunner 4 / e2e 1）
  全绿（-race）；e2e 全链 TestV0470AppCatalogDeployE2E
  （US-1 目录 + US-3 包 checksum + US-2 分批/离线尽力/回滚 + US-4 脚本内联/
  sha256 校验/install+start/伪 Pod 回穿）。
- 实现裁决留痕：
  ① Tick 扫描拾起语义（pending 新计划在首个扫描周期自动转 running 并推一批；
  paused 不拾起——Create 保持 pending 初始态可观测，e2e 曾捕获「pending 永挂」
  缺陷修复）；② 终态内联（批末汇总，最后一批同一扫描周期到终态）；
  ③ 回滚 = 对原 delivered 节点集再下发目标版本（PodSync update 覆盖同名 Pod
  等效回滚；显式 Remove 仅 Cancel 路径）；④ Deployer 契约要求并发安全
  （Cancel 卸载在独立 goroutine 调用）；⑤ 脚本包 ConfigSync 内联上限 8MB
  （spec §2 US-4 原文「script.tar.gz(base64)」按内联交付，超出 8MB 拒绝交付
  走离线预下发面——边界 ④ 32MB 指云端包存储上限，两者并存）；⑥ 伪 Pod 命名
  edgeflow-script-<app>（app 原样拼接，不做重复语义折叠）。
- env 清单：EDGEFLOW_CLOUDCORE_APPSTORE_FILE / _APPPKG_DIR /
  _APPDEPLOY_SCAN_SEC / EDGEFLOW_EDGED_APPPKG_DIR / EDGEFLOW_EDGED_SCRIPTRUNNER。
