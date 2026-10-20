# spec 0023 — 区域层与标签治理（v0.50.0，G03 + G04）

## 1. 背景与目标

规模阶段治理缺口：节点多起来后缺「组织维度」——按车间/园区/楼层归属节点、
按标签筛选资产。本版交付云端管理面的**区域树**与**标签 KV**，以及**按区域+
标签组合查询节点**。纯云端管理面（无新边云消息，消息数保持 19）。

对齐缺口编号：G03（区域层）/ G04（标签治理）。

## 2. 用户故事

- **US-1 区域树管理**：operator 创建区域（根「华东」→ 子「车间A」），改名/
  改描述/移动（换父），查询返回完整树；有子区域或有节点归属的区域不可删。
- **US-2 节点归属**：operator 把节点批量指派到区域（幂等换区，从旧区自动
  迁出）；删除区域前须先迁出节点。
- **US-3 标签打标**：operator 对 node/device/model 三类锚点打 KV 标签
  （幂等 upsert；null 值删标）。
- **US-4 组合查询**：viewer 按 region + tag 组合筛选节点（返回归属区域 +
  标签视图）。
- **US-5 越权拒绝**：viewer 调写面（建区/打标）→ 403；匿名 → 401。
- **US-6 持久化**：配置快照路径时重启后区域树与标签保留；纯内存模式重启
  易失（明示设计）。

## 3. 设计与裁决

1. **区域树单父**：`Region{ID, Name, ParentID, Description, CreatedAt,
   UpdatedAt}`；根 ParentID=""；深度/宽度无硬限（管理面量级 ≤ 数百）。
2. **移动防环**：改父时沿 newParent 链向上查自身，命中 → 409（环）；
   移到自身子树内同样被防环拦截。
3. **删除约束**：有子区域或有节点归属 → 409；空区域可删（其节点归属记录
   随区域删除，无孤儿）。
4. **节点单区域归属**：一个节点最多属一个区域；assign 幂等换区；**不校验
   节点在线/注册**（区域归属是静态管理面，允许预配置离线节点）——运行视角
   的 NodeInfo 不侵入（registry 保持运行时职责，治理面独立存储，查询时
   join 视图）。nodeIds 批量 ≤64；空 nodeId 跳过并计数返回。
5. **标签三类锚点隔离**：anchorType ∈ {node, device, model}；device 锚点
   ID 用复合串 `nodeID/namespace/deviceName`；**锚点存在性不校验**（打标面
   与资源面解耦；资源删除后标签残留为已知边界 → KI 登记，查询面本版只做
   节点故不影响）。
6. **标签 KV 语义**：`{k: v}` upsert 幂等；`{k: null}` 删标；单次批量
   ≤32 键；key ≤64B 且限定 `[a-zA-Z0-9._-]`；value ≤256B 任意 UTF-8。
7. **组合查询独立端点**：`GET /api/v1/governance/nodes?region=&tag=&tagKey=`
   ——不动既有 `/api/v1/nodes` 契约（只增不改）；region=区域ID、tag=key:value
   精确匹配、tagKey=仅按键存在过滤；三过滤可组合（AND）；响应按归属区域
   分组平铺 `{nodeId, region, tags}`。
8. **持久化**：JSONL 全量快照（写 temp + rename 原子替换），每次变更触发；
   env `EDGEFLOW_CLOUDCORE_REGION_FILE` / `EDGEFLOW_CLOUDCORE_TAG_FILE`
   可选路径，空 = 纯内存。**不做 etcd 写穿**（管理面量级不需要，与 logstore
   轻量持久化同口径；多副本共享留后续）。
9. **RBAC**：区域/标签写面（POST/PUT）= operator+；**DELETE（删区）= admin**
   （对齐 v0.46 既有语义「operator 无 delete」——只增不改红线，e2e run2
   实测抓到草案与矩阵冲突后修订）；查询 = viewer+。全局 middleware 按
   method 判定，零额外装配。scope.ns（授权范围维度）与区域（组织维度）
   **正交，本版不做联动**。
10. **ID 与时间**：区域 ID 服务端生成 `rg-` + 8hex；时间戳毫秒 UTC。

## 4. 端点（107 → 114，+7）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| POST | /api/v1/regions | 创建区域 | operator+ |
| GET | /api/v1/regions | 区域树（全量树形） | viewer+ |
| PUT | /api/v1/regions/{id} | 改名/描述/移动（防环） | operator+ |
| DELETE | /api/v1/regions/{id} | 删除（空区域） | admin |
| POST | /api/v1/regions/{id}/nodes | 节点批量归属（幂等换区） | operator+ |
| POST | /api/v1/tags | 打标/删标（三类锚点） | operator+ |
| GET | /api/v1/governance/nodes | 区域+标签组合查询 | viewer+ |

## 5. 非目标（本版不做）

区域级 RBAC 授权联动（scope 联动）；标签继承（子区域继承标签）；区域配额
与树深度限制；设备/模型锚点的组合查询（标签可打，查询面只做节点，按需扩）；
多区域归属；etcd 写穿与多副本共享；标签资源删除级联清理。

## 6. 交付锚（完成后回填）

- 端点 107→114 / 消息 19 不变；契约四件套对齐（routes/API-SPEC §1.1/
  API-COMPATIBILITY 矩阵 108-114 + 增量节/契约测试扫描清单 + 探测分支）。
- 单测：regionstore 7 例 + tagstore 7 例（含持久化 roundtrip/防环/删除约束/
  幂等 upsert/删标/三类锚点隔离/组合过滤）。
- e2e `TestV0500GovernanceE2E`：区域树（US-1）→ 归属（US-2）→ 打标（US-3）
  → 组合查询（US-4）→ 越权（US-5 viewer 写 403/匿名 401）→ 防环与删除约束
  （409×2）。
- 门禁：vet / race ×N 包 / 契约 / 定向 e2e / 全量 e2e。
- 复核（15min 硬时限）→ 处置回执 → 双提交推送。
- 实际交付（2026-10-20）：单测 14 例（regionstore 8 + tagstore 6）；契约复验
  14.399s 绿；定向 e2e run4 PASS 4.507s（run1 缺二进制/run2 DELETE 403 抓到
  spec 草案与 v0.46 RBAC 矩阵冲突 → 裁决 9 修订 DELETE=admin/run3 US-6 删除
  约束归因混杂 → 场景重排独立归因）；race ×3 包绿；全量 e2e 见 RN N5。
- 复核（PASS，6m37s，P2×2/P3×8）→ 处置后放行：P2 spec 表 DELETE 行对齐 +
  tagKey 存在性过滤补测；P3 快修 5（含 load 父链环检测断链防持锁死循环）留档 2。
  推送见 RN N5/N6。
