# EdgeFlow v0.46.0 发布说明（RBAC 与隔离 + 镜像凭证 + 运行时扫描评估）

- 发布日期：2026-10-08
- 基线：v0.45.0（ee954f2）
- 主题：RBAC 与隔离（G27）+ 镜像凭证托管与边缘拉取（G28）+ 运行时扫描评估
  （文档交付）。三角色授权矩阵（admin/operator/viewer + scope.ns 命名空间范围）、
  凭证哈希存储与 AES-256-GCM 静态加密（可选密钥）、私有镜像仓库凭证云端托管 →
  发布下发（authRef 形态，密码不进消息明文）→ 边缘 docker login 拉取。
  契约扩容 75→82 端点；零第三方依赖（crypto 标准库）；默认零行为
  （RBAC off 时认证语义与 v0.45 逐字节一致）。

## N1 能力（新增）

### 1. RBAC 角色与授权（G27）
- 角色三档：`admin`（全权）/ `operator`（读写、无删除、roles/users 面拒绝）/
  `viewer`（只读）；scope.ns 命名空间范围约束命名空间化资源（admin 显式
  scope 同样生效，空 = 不限）。
- 凭证：id + token SHA-256（constant-time 比对）+ role + scope.ns；JSON 文件
  持久化（0600 + 原子 rename 写穿）；明文 token 仅创建响应出现一次；撤销
  即时生效（内存表 + 文件同步）。
- 装配：`EDGEFLOW_CLOUDCORE_RBAC=on` + `EDGEFLOW_CLOUDCORE_RBAC_FILE=<path>`
  （on 而未配文件 → 拒绝启动 fail-fast）；**默认 off 零变化**——auth 单令牌
  语义保留（RBAC 凭证未命中时 env 单令牌回退为 admin 身份 "token"）。
- 授权中间件：401（未认证）/403 `{"error":"forbidden","required":"<op>"}`
  （不泄露存在性）；审计身份 = 凭证 ID（RBAC 命中）/"token"（env 回退）/
  anonymous（401），审计格式零变化。
- 角色管理面（admin-only）：`GET /api/v1/roles`（角色+权限矩阵说明）、
  `GET /api/v1/users`（脱敏列表 hashPrefix ≤12）、`POST /api/v1/users`
  （创建，明文 token 一次性返回）、`DELETE /api/v1/users/{id}`（撤销）。
  RBAC off 时端点存在但恒 403（契约面稳定，不泄露内部状态）。

### 2. 镜像凭证托管与边缘拉取（G28）
- 云端 imageauth 包：registry → {username, password}；password 仅存 SHA-256
  哈希 + 可选 AES-256-GCM 静态加密（`EDGEFLOW_CLOUDCORE_IMAGEAUTH_KEY` 派生；
  未配 key 仅存哈希 + Warn，下发链路明确报「下发不可用」）；文件 0600 写穿；
  明文不落盘/不回显/不进日志（列表 password = 哈希摘要前 12 位 + deployable）。
- 管理端点（admin-only）：`GET /api/v1/image-auths`、
  `PUT /api/v1/image-auths/{registry}`（body 明文 password 仅此一次）、
  `DELETE /api/v1/image-auths/{registry}`；未配置凭证文件时列表 200 空态
  （enabled=false）、写操作 503 未启用。
- 下发链：Deployer.ImageAuthLookup（依赖倒置接口）命中 → podsync.pod 新增
  可选字段 `imageAuth{registry,username}`（authRef 形态——密码不进消息明文，
  只增不改向后兼容）。
- 边缘 edged：docker_runtime 在容器创建前按需 `docker login <registry> -u
  <user> --password-stdin`（密码来自 `EDGEFLOW_EDGED_IMAGEAUTH` env JSON，
  不进 argv/日志；未配置该 registry → Warn + 匿名拉取；login 失败不阻塞
  run——私有仓库由 run 错误暴露并走既有重试）。

### 3. 运行时扫描评估（G28，文档交付）
- docs/SECURITY-GUIDE.md「镜像运行时扫描评估」：集成点（发布创建后钩子/
  边缘拉取后）、Trivy vs Clair 对比、缺口登记（镜像层带宽/边缘资源占用/
  离线漏洞库更新）、**不内置裁决**（保持零第三方依赖，产出对接规范留位）。

## N2 兼容与冻结
- **契约扩容**：75→82 端点（roles 1 + users 3 + image-auths 3）；消息 16 维持
  （podsync.imageAuth 可选字段=只增不改，旧边缘忽略未知字段）。
- **默认零行为**：RBAC off（默认）+ 无凭证文件 → 存量部署/测试行为逐字节
  不变（e2e A 段铁证）；auth env 单令牌路径完整保留。
- 冻结带 v0240–v0350 测试零改动；v0.39–v0.45 测试零改动；go.mod 零变化
  （AES-GCM 用标准库 crypto/aes+cipher）。

## N3 测试（grep 口径）
- cloud/pkg/rbac +4（角色矩阵 12 用例表驱动/scope.ns/Store 增删查+持久化重载/
  Middleware 401/403/回退/身份改写）。
- cloud/pkg/imageauth +3（Set/Lookup 回环+文件无明文+脱敏；无 key 降级；
  registry 规范化边界）。
- edge/pkg/edged +4（env JSON 解析/坏 JSON/空密码忽略/Pod imageAuth 指针
  回环 omitempty/login 空 username 拒绝）。
- cloud/pkg/audit：IdentityOf/MountSlot 对称导出（既有套件回归覆盖）。
- tests/e2e +1（TestV0460RBACImageAuthE2E：A 默认零行为 + B 角色矩阵/撤销
  即时生效 + C 503 未启用 + D 凭证全链脱敏/落盘无明文/删除 200/404）。
- 合计：新增 12 例（单测 11 + e2e 1）全绿。

## N4 边界（登记 KNOWN-ISSUES §47）
- scope.ns 仅约束命名空间化资源；凭证文件启动加载、后写不热加载（安全默认）；
  operator 无删除权（保守起步）；首个 admin 凭证需预写引导文件（表空不开放
  创建）；imageAuth 密钥依赖 env（未配则下发不可用）；边缘凭证 env 明文形态；
  运行时扫描仅评估不内置。

## N5 门禁（2026-10-08 全绿）
- [1] `go vet ./...`：VET OK。
- [2] 全仓测试（除 e2e/契约）：cmd/cloudcore **TestV0400Setpoint 重投 MsgID
  断言偶发 FAIL 一次**（既有 setpoint 时序偶发，与本版无关——本版未触碰
  setpoint 链路；与 v0.45 门禁同位置 mqttsim 偶发同模式）。单独复跑：定向
  1 次 + 全包 `-count=3` 全绿（诚实登记）。
- [3] `go test -race`（触碰 7 包：cloud/pkg/rbac / imageauth / audit /
  modelrelease / edge/pkg/edged / metamanager / cmd/cloudcore）：全绿
  （cloudcore 复跑确认）。
- [4] 契约 `go test ./tests/contract/`：13.014s 全绿（82 端点/16 消息；
  admin-only 端点 403 语义探测分支）。
- [5] e2e 全量 `go test ./tests/e2e/ -timeout 25m`：**645.640s 全绿（exit=0）**——
  含 v0.39–v0.45 零回归 + 新增 TestV0460RBACImageAuthE2E（A 零行为/B 角色矩阵
  /C 503/D 凭证全链 4.85s；单项先跑见 e2e-v0460-run1.log）。
- 门禁日志：`.cluster/edgeflow-v0460/gates.log`（含 [2] 偶发诚实登记与复验）
  + `e2e-final.log`。跑测前后 `lsof -i :12379,:12380` = 0。
