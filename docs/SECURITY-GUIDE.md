# EdgeFlow 安全指南：RBAC 与镜像凭证（v0.46.0，spec 0019 / G27+G28）

面向：部署与运维 EdgeFlow 云边平台的运维者、安全审计人员。

## 1. RBAC 启用与角色矩阵

### 1.1 启用步骤（opt-in，默认 off）
```bash
# ① 准备凭证文件（0600；token 为 SHA-256 hex——由明文计算）
#    明文 → 哈希：echo -n "<token>" | shasum -a 256
cat > /etc/edgeflow/rbac-users.json << 'EOF'
[
  {"id":"ops-admin","hash":"<sha256(admin-token)>","role":"admin"},
  {"id":"ops-deploy","hash":"<sha256(deploy-token)>","role":"operator"},
  {"id":"grafana-ro","hash":"<sha256(ro-token)>","role":"viewer","scopeNs":["monitoring"]}
]
EOF
chmod 600 /etc/edgeflow/rbac-users.json

# ② cloudcore 启动环境
export EDGEFLOW_CLOUDCORE_RBAC=on
export EDGEFLOW_CLOUDCORE_RBAC_FILE=/etc/edgeflow/rbac-users.json
```
- 开启但未配文件 → 拒绝启动（fail-fast）；文件后写不热加载（改后重启生效）；
- env 单令牌（`EDGEFLOW_CLOUDCORE_API_TOKEN`）保留为 admin 回退（向后兼容）；
  RBAC on 时建议撤下 env 单令牌或仅作 break-glass。

### 1.2 角色矩阵
| 操作 | admin | operator | viewer |
|---|---|---|---|
| GET 业务资源（nodes/models/pods/alarms/...） | ✅ | ✅ | ✅ |
| GET roles/users（角色管理面） | ✅ | ❌ 403 | ❌ 403 |
| POST/PUT 业务资源 | ✅ | ✅ | ❌ 403 |
| DELETE 业务资源 | ✅ | ❌ 403 | ❌ 403 |
| POST/DELETE users（凭证管理） | ✅ | ❌ 403 | ❌ 403 |
| 命名空间外资源（scope.ns 非空时） | 受约束* | 受约束 | 受约束 |

\* scope 显式即生效（含 admin）；需全权 admin 则创建时不带 scopeNs。

### 1.3 引导与撤销
- 首个 admin 凭证必须预写文件（表空 + env 未设 → 管理面 401——不存在
  「无凭证创建首个凭证」的引导漏洞）；
- 撤销：`DELETE /api/v1/users/{id}` 即时生效（内存表 + 文件同步写穿）；
- 轮换：创建新凭证 → 分发 → 删除旧凭证（双凭证平滑轮换）。

## 2. 私有镜像仓库凭证

### 2.1 云端托管
```bash
export EDGEFLOW_CLOUDCORE_IMAGEAUTH_FILE=/etc/edgeflow/imageauth.json
export EDGEFLOW_CLOUDCORE_IMAGEAUTH_KEY=<任意高熵口令>   # 可选；未配则仅哈希+下发不可用
```
```bash
# 设置（body 明文 password 仅此一次；响应/列表/日志均不回显）
curl -X PUT -H "Authorization: Bearer <admin-token>" \
  -d '{"username":"ops","password":"<pull-password>"}' \
  http://cloudcore:8080/api/v1/image-auths/priv.registry.io:5000
```
- 存储：password 仅存 SHA-256 哈希 + AES-256-GCM 密文（key 未配 → 仅哈希，
  列表 `deployable:false`，下发链路明确报「下发不可用」）；
- 列表脱敏：password 字段 = 哈希摘要前 12 位；文件 0600 + 原子写穿。

### 2.2 发布下发与边缘拉取
- 发布创建时按镜像 registry 前缀匹配凭证 → podsync.pod.imageAuth =
  `{registry, username}`（**authRef 形态：密码不进消息明文**）；
- 边缘环境变量（每个节点独立配置）：
```bash
export EDGEFLOW_EDGED_IMAGEAUTH='{"priv.registry.io:5000":"<pull-password>"}'
```
- 容器创建前按需 `docker login --password-stdin`（密码经 stdin，不进 argv/
  日志）；未配置该 registry → Warn + 匿名拉取；login 失败不阻塞 run
  （私有仓库由 run 错误暴露，走部署循环既有重试）。

### 2.3 验证清单
1. PUT 后响应无 password；列表 password ≤12 位且 ≠ 明文；文件无明文；
2. 发布含私有镜像 → 边缘收到 imageAuth{registry,username}（无 password）；
3. 边缘 docker login 日志无密码；容器可拉取启动；
4. 未配 KEY 时：列表 deployable=false，发布查表日志「下发不可用」。

## 3. 镜像运行时扫描评估（G28，登记评估——不内置）

| 维度 | Trivy | Clair |
|---|---|---|
| 形态 | 单二进制/离线 DB 更新 | 服务化（API + 索引器/匹配器） |
| 集成点 | 发布创建后钩子扫镜像 tar；边缘拉取后本地扫 | 需独立部署 registry 侧服务 |
| 边缘资源占用 | 低（按需扫描，CPU 峰值可控） | 高（常驻服务 + DB） |
| 离线场景 | 支持（DB 离线包） | 需内网 mirror |
| 第三方依赖 | 引入二进制 + DB 更新通道 | 引入服务 + DB |

**裁决**：v0.46 不内置扫描器（保持零第三方依赖）。候选集成路径：
①云端发布创建后钩子（Trivy 扫 mirror tar，高危阻断发布）——推荐首选；
②边缘拉取后本地扫（资源受限，仅登记不推荐）。落地前需评估：DB 更新通道
（离线部署合规）、扫描耗时对发布 SLA 影响、误报治理流程。

## 4. 与既有安全面的关系
- 认证（v0.21 Token）/ TLS（SAN 校验）/ nodeToken（云边注册）/ 审计台账
  （operator 身份槽）全部沿用；RBAC 是认证之后的授权层（401/403 语义集中）；
- 审计记录 operator = 凭证 ID（RBAC 命中）/"token"（env 回退）/anonymous
  （401）——审计格式零变化，可直接按凭证 ID 出审计报表；
- 云边消息面（config-sync/podsync）新增 imageAuth 仅 authRef（无密码）；
  消息对称加密（v0.2 起）不受影响。
