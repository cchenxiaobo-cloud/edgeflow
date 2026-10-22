# spec 0040 — $queue 队列订阅（v0.66.0，持续项 B5 相邻）

> 版本：v0.66.0 ｜ 缺口：MQTT 语义扩展——$queue 队列订阅（mqttsim 面）｜
> 上游：spec 0005（共享订阅）
> 说明：本版为 MQTT 仿真器语义扩展：零 API 变更（契约 139 维持）、零新增消息。

## 裁决

1. parseShareFilter 支持 "$queue/{inner}" → 固定组（"$queue"）共享订阅
2. 复用 $share 轮转机制（多订阅者负载均衡轮转接收）
3. granted-QoS 回显请求 QoS（与 $share 一致）
4. $queue 与 $share/{group} 各自独立轮转互不干扰
5. v3.1.1 冻结锚保留（$queue 在 v3.1.1 路径仍拒绝）
6. 非目标：$queue 离线暂存、$queue 与 $share 混合去重

## 交付锚

实现+测试详见 RN-v0660。
