# spec 0026 — 数据转发出口（v0.53.0，G18 起点）

> 版本：v0.53.0 ｜ 缺口：G18「与物联网平台/数据湖标准接口对接（数据转发出口）」首期 ｜
> 上游：spec 0025（v0.52 规模·韧性·基线）
> replan 验收锚：三场景端到端联调（本版先立转发出口语义，场景闭环在 v0.54）

## 背景与范围裁决

replan v0.53 规划「三场景端到端 + 平台对接」。本版先交付对接面的第一块：**告警事件
webhook 转发出口**（数据面转发留后续版本）——三场景闭环（v0.54）需要它作为
「告警通知外部系统」的标准出口。

**裁决：alarmstore 状态迁移挂点 → forwardstore 匹配启用规则 → 异步 webhook 投递
（HMAC-SHA256 签名 + 3 次重试）+ 送达台账。**

## 裁决

1. **规则模型**：{id, name(1..64B), targetUrl(http/https 绝对 ≤512B), secret(可选
   ≤128B), events(alarm.raised/acked/assigned/closed 非空子集), enabled, 时间戳}；
   快照持久化 EDGEFLOW_CLOUDCORE_FORWARD_FILE（空=内存，对齐治理面快照惯例）。
2. **挂点**：alarmstore.SetTransitionHook（单槽回调，锁外通知；事件名 = "alarm."+状态名）；
   Upsert 新告警 = raised，Ack/Assign/Close = 对应态；合并路径状态前移也发对应事件；
   终态后迟到重发不发（KI §41 语义对齐）。
3. **投递**：每规则独立 goroutine（panic 兜底不伤告警主流程）；POST JSON payload
   {event,alarmId,nodeId,state,severity,count,message,at}；签名头 X-EdgeFlow-Signature:
   sha256=<hmac(body,secret)>（无 secret 则不带）；单次尝试超时 5s；失败重试 ≤3
   （退避 500ms/2s）；2xx=delivered。
4. **台账**：每规则环形 100 条（内存+快照），记录 attempts/respCode/error/durationMs；
   台账随规则删除（长留档走审计台账口径，本包不承担）。
5. **API 6 端点**（122→128）：清单/详情=脱敏视图（secret 不回显，hasSecret 代替）；
   POST/PUT=operator+；**DELETE=admin**（本仓库首个 DELETE 面，v0.46 矩阵语义落地）；
   台账查询 viewer+；limit 默认 50 钳 [1,200]。
6. **只增不改**：既有 122 端点零改动；alarmstore 变更仅增 SetTransitionHook/notify
   （未设回调行为不变）；消息 19 不变。
7. **非目标**：数据面（设备时序）转发、同步测试投递端点、投递断路器/限流、
   MQTT/对象存储等其它 sink 类型、跨实例投递去重、异步投递的端到端可靠语义
   （尽力交付，失败可见于台账）。

## 端点（+6，3 写面 3 读面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| GET | /api/v1/forwards | 规则清单（脱敏） | viewer+ |
| POST | /api/v1/forwards | 创建规则 | operator+ |
| GET | /api/v1/forwards/{id} | 规则详情 | viewer+ |
| PUT | /api/v1/forwards/{id} | 部分更新 | operator+ |
| DELETE | /api/v1/forwards/{id} | 删除规则 | admin |
| GET | /api/v1/forwards/{id}/deliveries | 送达台账 | viewer+ |

## 数据依赖

- forwardstore（自包含快照）｜alarmstore 迁移挂点（SetTransitionHook）。

## 交付锚

- 实际交付（2026-10-21）：单测 8 例（forwardstore 7 + alarmstore 挂点 1，race 全绿）；
  契约复验 14.996s 绿（128 条，122→128）；定向 e2e run3 PASS 10.13s（run1
  waitNodeRegistered 401 → 带 token 轮询；run2 测试变量编译错修复；真告警全链投递
  +HMAC 逐字节校验）；全量 e2e 待 v0.56 后 R2 终验。
- 复核安排：v0.52+v0.53 合并 R1 独立复核检查点（回填
  .cluster/edgeflow-v052-056/review.md）。
