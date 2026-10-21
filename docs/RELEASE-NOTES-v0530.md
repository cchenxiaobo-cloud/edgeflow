# EdgeFlow v0.53.0 发布说明（数据转发出口）

> 版本：v0.53.0 ｜ 日期：2026-10-21 ｜ spec：0026-forwards ｜ 缺口：G18「数据转发出口」首期
> 基线：v0.52.0（3d033c8）。**本版为纯云端管理面版本：无新增边云消息（消息数保持 19）。**

## N1 能力

- **转发规则（新包 cloud/pkg/forwardstore）**：{name 1..64B / targetUrl http(s) 绝对
  ≤512B / secret 可选 ≤128B / events 四态事件非空子集 / enabled}；快照持久化
  EDGEFLOW_CLOUDCORE_FORWARD_FILE（空=内存，temp+rename 原子写，治理面惯例）。
- **迁移挂点**：alarmstore.SetTransitionHook（单槽回调，锁外通知；事件名 =
  "alarm."+状态名；Upsert 新告警=raised、Ack/Assign/Close=对应态、合并路径状态前移
  也发对应事件、终态后迟到重发不发——KI §41 语义对齐）。
- **异步投递**：每规则独立 goroutine（panic 兜底不伤告警主流程）；POST JSON payload
  {event,alarmId,nodeId,state,severity,count,message,at}；签名头 X-EdgeFlow-Signature:
  sha256=<hmac(body,secret)>（无 secret 则不带）；单次尝试超时 5s；重试 ≤3
  （退避 500ms/2s）；2xx=delivered。
- **送达台账**：每规则环形 100 条（内存+快照），记录 attempts/respCode/error/
  durationMs；台账随规则删除。
- **规则 API 6 端点（122→128）**：清单/详情=脱敏视图（secret 不回显，hasSecret 代替）；
  POST/PUT=operator+；**DELETE=admin（仓库首个 DELETE 面，v0.46 矩阵语义落地）**；
  台账查询 viewer+，limit 默认 50 钳 [1,200]。

## N2 兼容

- 端点 122→128（+6，3 写面 3 读面）：见 API-COMPATIBILITY §1 矩阵 123-128 与
  v0.53.0 增量节。
- 消息 19 不变；契约四件套同步（routes/API-SPEC §1.1/API-COMPATIBILITY/契约测试
  扫描清单 + forwards 前缀并入治理面探测语义）。
- **RBAC**：read=viewer+ / write=operator+ / delete=admin。
- alarmstore 变更**只增不改**：SetTransitionHook/notifyTransition 为纯新增，
  未设回调行为不变。

## N3 测试（新增 8 例 + e2e 五段）

- `cloud/pkg/forwardstore/v0530_test.go` 7 例：创建校验（7 类非法输入拒绝）/
  更新与删除（部分更新+secret 清除+同值 URL+未知 404+重复删除 404）/Dispatch+HMAC
  签名逐字节校验+payload 字段/失败重试（attempts=3、respCode=500、退避真实发生）/
  事件过滤与禁用/持久化 roundtrip（规则+台账+hasSecret）/台账环形 100（新→旧）。
- `cloud/pkg/alarmstore/v0530_test.go` 1 例：挂点事件序列（raised→acked→closed；
  合并重发无事件；终态迟到无事件）。
- e2e `TestV0530ForwardsE2E` 五段（**真告警全链**：edgecore mock 传感器+阈值规则；
  接收端为测试内 httptest）：US-1 创建规则+raised 投递（HMAC 签名逐字节校验）→
  US-2 ack/assign/close 生命周期三事件 → US-3 台账 delivered 新→旧 → US-4 脱敏
  +RBAC（viewer GET 200 无 secret 回显 / viewer POST 403 / 匿名 401 / operator PUT
  200 / viewer·operator DELETE 403 / admin DELETE 200）→ US-5 删除后详情与台账 404。
- e2e 修复轨迹：run1 waitNodeRegistered 不带认证（RBAC on → 401）→ 改带 viewer
  token 轮询；run2 测试变量未声明（编译错）→ **run3 PASS 10.13s**。

## N4 边界（已知限制，详见 KI §54）

- 尽力交付（异步无持久化重投队列；进程重启在途投递丢失，失败可见于台账）。
- 无同步测试投递端点；Secret 明文存快照（与治理面快照同安全域）；无断路器/限流。
- 台账随规则删除（长留档走审计台账口径）。

## N5 门禁

- `go build ./...` / vet（forwardstore/alarmstore/cloudcore/contract/e2e）/ gofmt：净。
- `go test -race`：forwardstore **7.929s** / alarmstore **4.530s** / hierarchyview 绿。
- 契约：**128 条复验 14.996s** 绿（122→128 + forwards 探测）。
- 定向 e2e：`TestV0530ForwardsE2E` **PASS 10.13s**（run3）。

## N6 复核

- 复核安排：v0.52 与 v0.53 **合并走 R1 独立复核检查点**（15min 硬时限），结论与
  处置回填 .cluster/edgeflow-v052-056/review.md；发现问题以 fix(review) 提交留痕。
- 本版门禁证据链：单测 race ×3 包 + 契约 128 条 + 定向 e2e run3（日志
  .cluster/edgeflow-v052-056/e2e-v0530-run3.log）。
