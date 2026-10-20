# EdgeFlow v0.50.0 发布说明（区域层与标签治理）

> 版本：v0.50.0 ｜ 日期：2026-10-20 ｜ spec：0023-region-tags ｜ 缺口：G03（区域层）+ G04（标签治理）
> 基线：v0.49.0（014d4e2）。**本版为纯云端管理面版本：无新增边云消息（消息数保持 19）。**

## N1 能力

- **区域树**（G03）：单父树形（`Region{ID,Name,ParentID,Description}`，ID 服务端生成
  `rg-<8hex>`）；创建/树查询（children+nodeCount）/更新（改名/描述/移动）/删除。
  移动防环（沿 newParent 父链查自身，含移入自身子树 → 409）；删除约束：有子区域
  或有节点归属 → 409，空区可删（归属记录随区域清理）。
- **节点归属**：节点批量指派到区域（批量 ≤64，幂等换区——旧区自动迁出，skipped
  计空 ID）；归属为**静态管理面数据**，不校验节点在线/注册（允许预配置离线节点）；
  registry 运行视角不侵入，查询视图在 API 层 join。
- **标签 KV**（G04）：三类锚点 `node` / `device`（复合 ID `nodeID/namespace/deviceName`）/
  `model` 隔离命名空间；`{k:v}` upsert 幂等、`{k:null}` 删标、空壳回收；批量 ≤32 键、
  key `[a-zA-Z0-9._-]` ≤64B、value ≤256B；锚点存在性不校验（打标面与资源面解耦）。
- **组合查询**：`GET /api/v1/governance/nodes?region=&tag=&tagKey=` ——region×tag
  AND 语义（交集）；无过滤 = 全量资产视图（归属节点 ∪ 有 node 标签锚点）；响应
  `{nodeId, region, tags}`。不动既有 `/api/v1/nodes` 契约（只增不改）。
- **持久化**：JSON 全量快照（temp+rename 原子替换），每次变更触发；env
  `EDGEFLOW_CLOUDCORE_REGION_FILE` / `EDGEFLOW_CLOUDCORE_TAG_FILE` 可选，空 = 纯内存
  （重启易失，明示设计）。Load 容错：孤儿归属（区域已不存在）自动清理。

## N2 兼容

- 端点 107→114（+7，全管理面）：见 API-COMPATIBILITY §1 矩阵 108-114 与 v0.50.0 增量节。
- 消息 19 不变；契约四件套同步（routes/API-SPEC §1.1/API-COMPATIBILITY/契约测试
  扫描清单 + 治理面探测分支）。
- **RBAC 语义对齐（设计要点）**：POST/PUT=write（operator+）、GET=read（viewer+）、
  **DELETE=delete（admin）**——对齐 v0.46 既有矩阵「operator 无 delete」（只增不改
  红线；e2e run2 实测抓到 spec 草案与矩阵冲突后修订 spec 裁决 9）。
- scope.ns（授权范围维度）与区域（组织维度）正交，本版不联动。

## N3 测试（新增 14 例）

- `cloud/pkg/regionstore/v0500_test.go` 8 例：创建+树形/更新移动/防环（自环+子树环）/
  删除约束（子/节点两型）/批量归属+幂等换区+超限/持久化 roundtrip（含孤儿归属清理）/
  校验面/并发读写。
- `cloud/pkg/tagstore/v0500_test.go` 6 例：upsert 幂等+变更计数/null 删标+空壳回收/
  三类锚点隔离/校验面（9 拒绝路径+32 键边界）/持久化 roundtrip/并发读写。
- e2e `TestV0500GovernanceE2E` 六段：US-1 区域树（建/树/移/删）→ US-2 归属（批量/
  skipped/换区迁移）→ US-3 打标（upsert/幂等/删标/坏 key 400）→ US-4 组合查询
  （tag/AND 命中/AND 排除/全量/未知区 404）→ US-5 越权（viewer 建区 403/viewer 打标
  403/viewer 删区 403/匿名 401/viewer 读 200）→ US-6 防环+删除约束（自环 409/子树环
  409/有子 409/有节点 409/迁出后删 200/级联后删根 200）。
- e2e 修复轨迹：run1 缺二进制（补 buildBinaries）→ run2 DELETE 403（v0.46 operator
  无 delete 语义，spec 裁决 9 修订 + DELETE 收敛 admin）→ run3 US-6 删除约束归因混杂
  （环链 B 区带子 C——场景重排：先移 C 到根再做独立归因）→ **run4 PASS 4.507s**。

## N4 边界（已知限制，详见 KI §51）

- 资源删除后标签残留（锚点存在性不校验的设计代价；本版组合查询只做节点面，无实际
  暴露；设备/模型锚点查询按需扩）。
- 快照写入在存储锁内（变更持锁写文件）——管理面量级（区域 ≤ 数百）IO 短可接受；
  异步化留后续。
- 快照为单文件全量态：多副本共享治理面不支持（各自快照独立，etcd 写穿留后续）。
- 区域树无深度/宽度硬限（管理面量级假设）；区域配额、标签继承（子区域继承）、
  区域级 RBAC scope 联动均为非目标。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race`：regionstore 1.421s / tagstore 1.792s / cmd/cloudcore 21.817s。
- 契约：**114 条复验 14.399s** 绿（107→114 + 探测分支）。
- 定向 e2e：`TestV0500GovernanceE2E` **PASS 4.507s**（run4）。
- 全量 e2e：**723.868s exit=0**（复核处置后终验，.cluster/edgeflow-v0500/e2e-full-v0500.log）。

## N6 复核（15min 硬时限，实际 6m37s，-race 复跑两 store 包 + 契约 114 条复验）

- 原始结论：**PASS（无 P1；P2×2 / P3×8）**；处置后全绿放行（回执
  .cluster/edgeflow-v0500/review.md，原始报告 review-original.md）。
- **P2×2 全修**：①spec §4 表 DELETE 行 operator+ → admin（与裁决 9 一致，仅表漏改）；
  ②tagKey 存在性过滤补测（tagstore 单测 2 断言——spec 裁决 7 广告语义此前零测试）。
- **P3**：快修 5（regionstore 头注释对齐「持锁内写快照」/治理 API 头注释 RBAC 对齐
  v0.46 矩阵/**load 后父链环检测断链**（手改快照防 isDescendant 持锁死循环）/
  `tag=:value` 空键显式 400/API-COMPATIBILITY 补 delete=admin + e2e 补 operator
  DELETE 403）；留档 2（契约探测 405 偏松/assigned 计数口径）→ KI §51#7/8。
- 复核确认关键面：RWMutex 无死锁/防环与删除约束双覆盖/RBAC 包装顺序/契约 {id}
  命名/消息 19 未动/快照原子写与孤儿清理。
- 修正后复验：单测绿（regionstore 1.096s/tagstore 1.471s）+ 定向 e2e run5
  **PASS 3.683s** + 全量 e2e **723.868s exit=0**。
