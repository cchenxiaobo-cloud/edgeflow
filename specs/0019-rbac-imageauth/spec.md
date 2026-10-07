# EdgeFlow v0.46.0 规格（spec 0019）——RBAC 与隔离 + 镜像凭证 + 运行时扫描评估

版本：v0.46.0（发展规划 G27 + G28）。状态：设计定稿（as-built 回填待实现后）。
基线：v0.45.0（ee954f2）；契约基线 75 端点 / 16 消息。

## 1. 背景与目标

安全差距评估（对齐智能边缘计算平台方案 §安全）：S2 命名空间隔离/RBAC 缺失（G27）、
S4 镜像凭证缺失 + 运行时扫描缺（G28）。现状：单一共享 API Token（持有即管理员，
v0.1 语义）、审计台账已有身份槽（operator）、云边注册 nodeToken 双向兼容、
Pod 部署为 docker run（无凭证支持——私有仓库拉取会失败）。

本版目标（验收三支柱）：
1. **角色矩阵**：admin/operator/viewer 三角色 + 权限点判定 + 越权 403；
2. **opt-in 零变化**：RBAC off 时存量部署/测试行为逐字节不变（认证语义维持）；
3. **凭证链路**：私有仓库凭证云端托管（脱敏）→ 发布下发 → 边缘 docker login 拉取。

## 2. 用户故事

### US-1 角色与权限模型（G27）
作为平台管理员，我配置多 API 凭证（凭证 ID + Token + 角色 + 可选命名空间范围），
替代单一共享令牌：
- 角色三档：`admin`（全权）/`operator`（读写除角色与凭证管理面）/`viewer`（只读）；
- 凭证持久化：JSON 文件（EDGEFLOW_CLOUDCORE_RBAC_FILE），Token 仅存 SHA-256
  哈希（明文只在创建响应出现一次）；scope.ns 空数组 = 不限；
- 下发阶段（云端启动）由 staticusers 包加载；env 单令牌路径保留（优先级见 US-2）。

### US-2 认证与授权判定（G27）
作为调用方，我持 Bearer 凭证访问 API，服务端按角色矩阵判定：
- 匹配规则：先查 RBAC 凭证表（hash 比对，constant-time），未命中回退 env 单令牌
  （命中 → admin 身份，向后兼容）；都未命中 → 401（语义不变）；
- 授权判定（authz.Can(role, op, ns)）：
  - admin：全部允许；
  - operator：GET/POST/PUT 全允许；DELETE 仅非角色管理面（角色/凭证 API 拒绝）；
  - viewer：仅 GET 允许；写操作一律 403；
  - scope.ns 非空时：请求目标命名空间不在范围内 → 403（仅约束命名空间化资源）；
- 越权响应：`403 {"error":"forbidden","required":"<op>"}`（不泄露存在性）；
- **默认 off 零变化**：EDGEFLOW_CLOUDCORE_RBAC=off（默认）时整个 RBAC 层不装配，
  auth 中间件行为与 v0.45 逐字节一致。

### US-3 权限点映射（G27）
权限点按「方法 + 面」映射，不逐端点维护（低基数）：
- 面（facet）判定：/api/v1/roles|users → `roles` 面（角色管理，最高敏感）；
  其余按方法 → read/write/delete；
- 语义：viewer=read；operator=read+write（delete 白名单外）；admin=*；
- 命名空间资源（devices/pods 等 path 含 /namespaces/{ns}/）受 scope.ns 约束；
  非命名空间资源（models/nodes 等）不受约束（platform 级）。

### US-4 角色管理 API（G27，admin-only）
- `GET /api/v1/roles`（角色枚举 + 权限矩阵说明）；
- `GET /api/v1/users` / `POST /api/v1/users`（创建凭证：id/role/token/scope.ns；
  响应含明文 token 一次）/ `DELETE /api/v1/users/{id}`（撤销）；
- 契约 +4 端点（75→79）；全部 admin-only（viewer/operator 访问 → 403）。

### US-5 审计衔接（G27）
- 审计 operator 记录凭证 ID（RBAC 命中）/"token"（env 回退）/anonymous（401）——
  沿用既有身份槽，审计格式零变化；
- 越权 403 由审计中间件自然落盘（action/path/operator/result 既有字段）。

### US-6 镜像凭证托管与下发（G28）
作为运营者，我为私有仓库配置拉取凭证，发布时随发布配置下发边缘：
- 云端：`imageAuthStore`（JSON 文件，EDGEFLOW_CLOUDCORE_IMAGEAUTH_FILE）——
  registry 地址 → {username, password}；password 仅存 SHA-256 哈希 + AES-GCM
  静态加密（密钥 EDGEFLOW_CLOUDCORE_IMAGEAUTH_KEY 派生，未配 key 仅存哈希并
  Warn「下发不可用」）；API：`PUT /api/v1/image-auths/{registry}`（body 含明文
  password 一次）/ `GET /api/v1/image-auths`（列表脱敏：password 字段回哈希
  摘要前 12 字符）/ `DELETE /api/v1/image-auths/{registry}`（契约 +3 → 82）；
- 下发：发布创建时按镜像 registry 前缀匹配凭证，注入发布项
  `imageAuth: {registry, username}`（密码不进 config-sync 明文——边缘已配对等
  凭证时跳过；未配对时 config-sync 传 authRef，边缘按 registry 本地查）；
- 边缘：edged docker_runtime `docker login <registry> -u <user> --password-stdin`
  （密码来自 EDGEFLOW_EDGED_IMAGEAUTH 环境变量 JSON：{registry:password}，
  逐实例部署前按需 login；未配置且镜像需要认证 → 容器创建失败带明确错误）；
- **脱敏硬规则**：所有日志/审计/响应不出现明文 password（只允许创建/设置请求体
  出现一次；哈希摘要回显 ≤12 字符）。

### US-7 运行时扫描评估（G28，文档交付）
- docs/SECURITY-GUIDE.md 新增「镜像运行时扫描评估」节：集成点（发布创建后钩子 /
  边缘拉取后）、候选方案（Trivy/Clair 对比）、缺口（镜像层拉取带宽、边缘资源
  占用、离线场景漏洞库更新）、结论（登记评估，不内置——保持零第三方依赖裁决，
  产出对接规范留位）。

## 3. 兼容与边界

- **契约只增不改**：+7 端点（roles 1 + users 2 + image-auths 3 + 存量零变化），
  75→82；消息零新增（发布配置增量可选字段 imageAuth，只增不改）。
- **默认 off 零变化**：RBAC off（默认）+ 无凭证文件 → 存量部署/测试行为不变；
  auth env 单令牌路径保留。
- 冻结带（v0240–v0350）测试零改动；go.mod 零变化（AES-GCM 用标准库 crypto/aes）。
- 边界登记：①scope.ns 仅约束命名空间化资源；②密码静态加密依赖环境密钥
  （未配则仅哈希+Warn）；③运行时扫描仅评估不实现；④docker login 失败重试
  由部署循环既有重试语义承接；⑤Token 撤销即时生效（内存表 + 文件写穿）；
  ⑥凭证文件权限 0600（创建时强制）。

## 4. 验收口径
- 角色矩阵：三角色 × 读/写/删 × 命名空间内/外 → 403/200 判定表驱动测试全绿；
- opt-in 零变化：RBAC off 时 auth/audit 既有测试零改动全绿（冻结证明）；
- 凭证链路：设置→脱敏回显→发布下发→边缘 login 拉取（e2e 用本地 registry 模拟
  或 login 命令 mock）→ 撤销。


## 5. 测试锚（as-built 回填）

- cloud/pkg/rbac/v0460_test.go：4 例（角色矩阵 12 用例表驱动/scope.ns 约束/
  Store 增删查+脱敏+持久化重载/Middleware 认证授权一体 401/403/回退/身份改写）。
- cloud/pkg/imageauth/v0460_test.go：3 例（Set/Lookup 解密回环/文件无明文/
  脱敏回显/覆盖写/非法地址；无 key 降级与「下发不可用」；registry 规范化边界）。
- edge/pkg/edged/v0460_imageauth_test.go：4 例（env JSON 解析/坏 JSON/空密码
  忽略/Pod imageAuth 指针回环与 omitempty/login 空 username 拒绝）。
- cloud/pkg/audit：IdentityOf/MountSlot 对称导出（既有套件回归覆盖）。
- tests/e2e/v0460_rbac_imageauth_e2e_test.go：1 例
  （TestV0460RBACImageAuthE2E：A 默认零行为 + B 角色矩阵/撤销即时生效 +
  C 503 未启用语义 + D 凭证全链脱敏/落盘无明文/删除 200/404）。
- 合计 12 例（单测 11 + e2e 1；grep -c '^func Test' 口径）。