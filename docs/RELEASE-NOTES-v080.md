# EdgeFlow v0.8.0 发布说明（运维与安全增强）

- **发布日期**：2026-08-26
- **版本基线**：v0.7.0（2026-08-25）→ v0.8.0
- **主题**：运维与安全增强——外部 etcd RBAC 鉴权透传（L1）+ 续约失败监控（L12）+ 模型列表分页与终态发布 GC（L28）
- **兼容性**：API 只增不改（既有 31 端点逐字节不变，列表端点新增可选查询参数）；env 只增不改；升级零迁移；边缘节点零动作

## 一、核心能力

### 1. 外部 etcd RBAC 鉴权透传（L1，KNOWN-ISSUES §5⑤ 闭环）

| 项 | 说明 |
|---|---|
| 新 env | `EDGEFLOW_CLOUDCORE_ETCD_USERNAME` / `EDGEFLOW_CLOUDCORE_ETCD_PASSWORD`（外部模式，必须成对设置） |
| 语义 | clientv3 Username/Password 透传（etcd RBAC 用户名密码鉴权）；与 TLS/mTLS 正交（可单独使用，可叠加 mTLS） |
| 校验 | 只设其一 → 启动 fail-fast（对齐 TLS_CERT/KEY 惯例）；embed/纯内存显式设置 → Warn 忽略（不串扰 M2 锚点） |
| 探活文案 | PermissionDenied 引导升级：① etcd 侧 `/edgeflow/` 前缀授权（etcdctl role grant-permission）② RBAC 用户名密码成对配置 ③ mTLS CN→角色映射（--client-cert-auth） |
| CN→角色映射 | 不新增透传项（etcd 侧 --client-cert-auth 已覆盖），文档指引 DEPLOYMENT §10.7.4 |
| Chart | `cloudcore.etcd.external.auth.{username,password}`（生产建议 K8s Secret 注入，勿明文写 values） |

### 2. 续约失败监控（L12 闭环）

- `/metrics` 新增 **`edgeflow_cloudcore_lease_renewal_failures_total`**（counter）——外部 etcd 模式心跳租约续约失败累计计数
- 仅外部模式注入（其余形态指标行不输出，5 项基线输出不变）；0 值也输出（监控面板基线）
- 告警建议：持续增长（如 5 分钟窗口内递增）→ etcd 侧异常/网络分区，阈值按判活 TTL 折算
- 指标总数：5 → 7（5 gauge + http 请求 counter + 续约失败 counter）

### 3. 模型列表分页与终态发布 GC（L28/N-4 闭环）

**分页（零破坏，缺省全量）**：

| 端点 | 新增参数 | 响应头 |
|---|---|---|
| `GET /api/v1/models` | `limit`（1-1000）/ `offset`（≥0） | `X-Total-Count` |
| `GET /api/v1/models/{modelName}/versions` | 同上 | 同上 |
| `GET /api/v1/models/{modelName}/releases` | 同上 | 同上 |

- 不传参数 = 旧行为（全量返回）；非法值（limit=0/1001/非数字、offset 负数）→ 400

**终态发布 GC（默认关闭）**：

| 项 | 说明 |
|---|---|
| 新 env | `EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED`（`1` 开启，默认关）/ `EDGEFLOW_CLOUDCORE_RELEASE_GC_KEEP`（默认 100，≥1） |
| 语义 | 发布进入终态（succeeded/failed/canceled/rolled_back）后，按 CreatedAt 保留最近 keep 条终态，删除更旧的终态头 + 逐节点结果；非终态/在途绝不删除 |
| 默认关闭 | 保持 L31 审计口径（终态 release 键永久保留）；开启后审计痕迹以 ops 台账为准 |
| 三模式 | 内存/embed/外部统一受益（纯内存模式防长运行内存线性增长） |

## 二、升级注意

- **零迁移**：v0.6.0/v0.7.0 → v0.8.0 直接升级（只新增 env 与可选查询参数，键空间无变化；`/edgeflow/models/` 前缀不动）
- **默认行为不变**：不设新 env → 与 v0.7.0 逐字节一致（GC 默认关、无鉴权、指标多一行但仅外部模式注入）
- **混跑禁令延续**：升级/回滚全停再全起（scale 0→1），禁止混合版本多副本混跑（v0.5.0×v0.6.0 误删活节点 L15；v0.6.0×v0.7.0 未验证 L29；v0.7.0×v0.8.0 同理未验证）
- **回滚**：v0.8.0 回 v0.7.0 零脏键（新功能无键空间变更）；GC 开启过的集群可选 `etcdctl del /edgeflow/models/releases --prefix` 已按需清理
- **AUTH=on**：模型 API 与既有端点一致自动要求 Bearer Token（401）
- **生产建议**：外部模式开启鉴权时——etcd 侧最小权限角色（readwrite `/edgeflow/`）+ mTLS（推荐）；密码经 K8s Secret 注入，勿明文写 values/环境

## 三、行为变化与语义知悉

| 面 | 变化 |
|---|---|
| 列表响应 | 分页参数下响应体为切片页 + `X-Total-Count` 头（缺省全量，既有客户端零影响） |
| /metrics | 外部模式多一行续约失败计数（新增输出，不改变既有行） |
| 终态发布 | GC 开启后旧终态（超 keep）被清理——查询历史发布依赖 perNode 明细/台账（默认关闭无此变化） |

## 四、验证摘要（实测）

- 全量 `go test -race ./...` 34 包全绿；go vet 干净
- 新增测试：`TestConfigAuth`（L1 五用例）、`TestLeaseRenewalFailuresMetric`（L12 三用例）、`TestMemoryGCReleases`（L28：保留/级联删/非终态保留/keep<1/幂等）、`TestModelAPIPagination`（分页/X-Total-Count/非法 400/缺省全量）
- helm lint 0 failed；外部模式渲染验证（auth env + MULTI_REPLICA 注入正确）
- 12 制品交叉编译 + tgz + sbom + checksums 构建通过

## 五、文档同步

API-SPEC（§1.1 分页/§1.2 指标/env 口径）、DEPLOYMENT（§10.7 env 表 + 鉴权配置）、KNOWN-ISSUES（§8 闭环登记）、README（当前版本/版本历史）、用户手册 v0.8.0（附录 A env + 相关章节）、解决方案手册 v1.2.0（外部 etcd 安全节 + 模型运营性）

## 六、遗留（非阻断，见 KNOWN-ISSUES §8 与 v0.7.0 §v0.8+ 候选延续）

- CN→角色映射透传项未做（etcd 侧配置覆盖，文档指引）；hb 键重建计数未做（自愈可观测性增强可后续加）
- R-1 发布前镜像存在性探活（P2，registry API 依赖外部网络，E2E 不可行，延后）；D6 批内并发（改变灰度语义，风险高，延后）
- 训练平台/模型评测/A-B 按请求切流（范围外延续）
