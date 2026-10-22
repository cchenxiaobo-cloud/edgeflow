# spec 0032 — 转发出口增强：测试投递与 MQTT sink（v0.59.0）

> 版本：v0.59.0 ｜ 缺口：KI §54#2 反转（测试投递）+ 通知渠道扩展第一步 ｜
> 上游：spec 0026（v0.53 转发出口）
> 说明：端点 133→134（+1）；Rule schema 只增字段（sinkType/topic，旧快照向后兼容）。

## 背景与范围裁决

转发出口（v0.53）落地后两个运维痛点：①规则配置后只能等真实事件验证连通性
（KI §54#2）；②webhook 之外缺第二通知渠道。本版收口：

**裁决：POST /api/v1/forwards/{id}/test 同步测试投递（绕过事件过滤）+
Rule.sinkType 扩展（webhook|mqtt），mqtt sink 经 pkg/mqtt 短连接 QoS1 发布。**

## 裁决

1. **测试投递**：POST /api/v1/forwards/{id}/test（operator+）——同步执行一次
   连通性投递（事件名 "test"，绕过事件过滤与告警源），payload 为合成事件
   {event:test, alarmId:test-<id>, message:连通性测试投递}；结果（delivered/
   attempts/status/error/deliveryId）同步返回并入台账（Event=test）；禁用规则
   400（ErrInvalid）、未知 404。
2. **sinkType 扩展**：Rule 增 sinkType（webhook 默认|mqtt）+ topic（mqtt 主题，
   空=edgeflow/forward/<id>）；地址语义分叉——webhook = http(s) 绝对地址（既有），
   mqtt = [tcp://]host:port（无路径）；schema 只增字段（旧快照 Load 默认 webhook，
   向后兼容）。
3. **mqtt sink 投递**：pkg/mqtt 短连接（每次投递 dial→PUBLISH QoS1→等
   PUBACK→DISCONNECT；ClientID=edgeflow-forward-<ts 随机>）；成功 = QoS1 确认
   完成；重试沿用 ≤3 次退避；HMAC 签名不适用（broker 侧认证另行处理，登记）。
4. **测试投递也走双路**：sinkType=webhook → HTTP POST（含签名）；mqtt → QoS1
   发布；台账 Event=test 可与真实事件区分。
5. **只增不改**：既有 133 端点零改动；台账 schema 不变（Event=test 为新取值）；
   消息 19 不变（mqtt sink 为出站外部协议）。
6. **非目标**：mqtt 长连接池、broker 认证（用户名密码/TLS——登记 KI）、
   重发退避曲线、payload schema 版本化、对象存储 sink。

## 端点（+1，写面）

| Method | Path | 语义 | RBAC |
|---|---|---|---|
| POST | /api/v1/forwards/{id}/test | 同步测试投递 | operator+ |

## 交付锚

- 实际交付（2026-10-22）：单测 4 例（测试投递/禁用与未知守卫/mqtt sink
  mqttsim 端到端/sinkType 校验，race 6.117s 绿含既有全过）；契约复验
  （134 条，133→134）；定向 e2e PASS 4.07s（webhook test 同步返回 + 合成
  事件 + 台账 test 条目 + 禁用 400）。
- 附：RN-v0580 N5 门禁数字补录（上版漏回填）：契约 133 条 16.320s +
  定向 e2e run2 10.84s。
- R3（v0.57–v0.59 合并复核）随本版推送后执行。
