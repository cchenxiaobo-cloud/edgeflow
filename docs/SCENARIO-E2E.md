# 三场景端到端验收（G29）

> v0.54.0（spec 0027）：replan 三场景主链路的平台级端到端验收。三条场景 e2e 全部
> 走真实 cloudcore+edgecore 进程 + RBAC 环境（非单测替身）；本文档记录验收标准、
> 链路断言与验收记录（如实登记，含等价物声明）。

## 场景与验收标准

### 场景 A — 告警通知闭环（视频告警等价）

| 项 | 内容 |
|---|---|
| 业务语义 | 现场 AI/阈值产生告警 → 平台告警中心 → 区域汇聚可见 → 通知外部系统 |
| 输入 | edgecore mock 传感器（温度穿越 28）+ 阈值规则（critical）+ 区域归属 + webhook 转发规则（订阅 alarm.raised） |
| 链路断言 | ① 真告警入库（alarmstore）；② hierarchy summary 区域 alarmActiveCount≥1（递归聚合）；③ webhook 送达接收端且 X-EdgeFlow-Signature 与规则密钥逐字节一致 |
| 等价物声明 | 视频告警与设备告警在平台内同为 Alarm 事件（source 区分）；视频接入/流媒体链路已有独立 e2e（v0.42–v0.44），本场景聚焦「告警产生后」的平台链路 |
| e2e | `TestScenarioAAlarmNotifyLoop` |

### 场景 B — 维护预警闭环

| 项 | 内容 |
|---|---|
| 业务语义 | 设备预警（warning）→ 确认 → 派单维护班（工单号）→ 闭环关闭；区域告警矩阵逐态可见 |
| 输入 | 预警阈值规则（warning lt 28）+ 区域归属 |
| 链路断言 | ① 预警入库（raised）；② ack → 区域矩阵 acked≥1；③ assign（维护班 + ticketRef=GD-SCEN-B）→ assigned≥1；④ close → closed≥1；⑤ 告警终态 closed |
| e2e | `TestScenarioBMaintenanceLoop` |

### 场景 C — 优化设定值闭环

| 项 | 内容 |
|---|---|
| 业务语义 | 优化目标值下发 → 审批 → 边缘执行 → 回告；拒绝路径留痕 |
| 输入 | 设定值建单（requireApproval=true，targetTemp=26.5） |
| 链路断言 | ① 建单 → approve → 边缘执行 → SetpointResult 回告 → applied；② 第二单 reject → rejected 留痕终态 |
| e2e | `TestScenarioCSetpointLoop` |

## 验收记录

| 日期 | 版本 | 结果 | 备注 |
|---|---|---|---|
| 2026-10-21 | v0.54.0 | 三场景 e2e 全绿（本机回环，真实进程 + RBAC 三凭证） | 见 RN-v0540 N5 门禁数字 |

## 边界与说明

- 触发依赖 mock 传感器穿越阈值频率（e2e 轮询上限 90s，实测 ~10s 量级）。
- 通知渠道本期仅 webhook 形态（HMAC-SHA256 签名）；短信/邮件等渠道由业务侧基于
  同一出口扩展（spec 0027 非目标）。
- 外部工单系统真实集成不在本期（assign 的 assignee/ticketRef 为平台侧留痕字段）。
