# EdgeFlow v0.1.1 监控告警配置（Monitoring & Alerting）

> 状态：✅ 已入档（2026-08-18；发布准备员起草，报告核验员核验口径一致，主线修正 3 处：checksums 12 条目、36 包口径、灰度走 keadm batch）｜阈值需在灰度观察期按实测回填校准。
> 依据：docs/ARCHITECTURE.md §5.6（可观测性）、docs/PERFORMANCE-BASELINE.md（基线值）、
> docs/SECURITY.md §5（CRL/OCSP 吊销闭环）、docs/DEPLOYMENT.md §2.4（探针参数）、
> docs/API-SPEC.md（/ocsp 端点）。
> 配套：release-prep-v011.md §5 观察期执行、rollback-runbook-v011.md §1 告警触发条件。

---

## 1. 现有指标盘点（/metrics 五指标）

> 云端 `/metrics`（Prometheus 文本格式，纯标准库实现，无 client 库依赖；
> 边缘侧不独立暴露指标——ARCHITECTURE.md §5.6 / R11）。Prometheus 直接 scrape 即可。

| 指标 | 类型 | 含义 | 备注 |
|------|------|------|------|
| `edgeflow_cloudcore_nodes_total` | gauge | 已注册边缘节点总数（**含离线**） | 无法直接区分 Ready/Offline（见 §2.5） |
| `edgeflow_cloudcore_pods_total` | gauge | 云端 Pod 状态记录总数 | — |
| `edgeflow_cloudcore_devices_total` | gauge | 云端设备状态记录总数 | — |
| `edgeflow_cloudcore_active_connections` | gauge | CloudHub 活跃连接数 | 掉到 0 = 全部边缘断连 |
| `edgeflow_cloudcore_http_requests_total` | counter | HTTP 请求计数，按**路由模式 + 状态码**分桶 | 路由模式非实际路径（防高基数）；**429 分桶天然可用** |

- 采集配置（prometheus.yml）：

```yaml
scrape_configs:
  - job_name: edgeflow-cloudcore
    metrics_path: /metrics
    static_configs:
      - targets: ['127.0.0.1:8080']   # 生产为 Service DNS：edgeflow-cloudcore:8080
    scrape_interval: 15s
```

---

## 2. 新增告警建议（v0.1.1 安全加固对应项）

| # | 告警目标 | 数据来源 | 与 v0.1.1 修复项对应 |
|---|---------|---------|---------------------|
| 2.1 | **/ocsp 限流 429 计数** | 现有 `http_requests_total{path="/ocsp",code="429"}` 增量 | S1（/ocsp 限流）：限流触发 = 潜在滥用/攻击信号，必须告警 |
| 2.2 | **CRL 锁降级 Warn 日志** | 日志告警（cloudcore 日志流，loki/mtail） | S3（CRL 降级日志）：CRL 不可用降级放行 = 吊销失效，warning 级 |
| 2.3 | **证书即将过期** | `openssl x509 -enddate` 巡检（textfile collector 或 cron 脚本） | 叶子证书 1 年/CA 10 年（SECURITY.md §2）：过期 = mTLS 握手全断 |
| 2.4 | **crl.pem nextUpdate 过期** | `openssl crl -nextupdate` 巡检 | 吊销产物过期 = 撤销状态不可信 |
| 2.5 | **心跳离线节点数** | 短期：blackbox exporter 探测 `/api/v1/nodes`；建议后续补 `nodes_offline_total` gauge | 基线 100% 在线（PERFORMANCE-BASELINE.md）；配合 NodeController 180s 判定 |

### 2.5 补充说明（指标缺口）

`nodes_total` 含离线节点，无法单独表达离线数。v0.1.1 不新增指标（patch 原则），
短期以 API 探测兜底；建议 v0.1.x 后续补 `edgeflow_cloudcore_nodes_offline_total`（gauge，
由 registry 按节点状态计数注入，与现有 Providers 注入机制同构——cloud/pkg/metrics/metrics.go）。

### 2.6 v0.11.0 指标补全（L12+，hb 键修复性重建计数）

外部 etcd 模式 /metrics 由 7 项增至 **8 项**（embed/纯内存保持 7 项）：

- `edgeflow_cloudcore_lease_hb_rebuilds_total`（counter，仅外部模式注入，**0 值也输出**）：
  hb 键被删/缺失后由续约 worker **成功重建**的次数（本副本仍在服务但键丢失的场景：
  applyDelete locallyServing / rescanOnce / gcSweepOne 守卫 0 三处修复性入口）。
- **告警建议**：持续增长（如 5min 内 >N）→ 租约抖动 / hb 键被外部删除（etcdctl 误删、他副本异常）
  ——与 `lease_renewal_failures_total`（etcd 侧异常/网络分区）**互补**：前者偏"键被外部干预"，
  后者偏"连接/服务故障"；两者同时增长 = etcd 侧异常。
- 阈值参考：判活 TTL（默认 300s）内允许少量重建（<TTL 窗口自愈）；>N/5min 持续 → 告警。

---

## 3. Prometheus 告警规则草案（YAML 片段）

```yaml
# 文件：prometheus-alerts/edgeflow-v011.yml
groups:
  - name: edgeflow-v011
    rules:
      # ---- 可用性（critical）----
      - alert: CloudCoreDown
        expr: up{job="edgeflow-cloudcore"} == 0
        for: 1m
        labels: { severity: critical, component: cloudcore }
        annotations:
          summary: "cloudcore 实例不可达（scrape 失败）"
          runbook: "docs/ROLLBACK-RUNBOOK-v011.md#3-①"

      - alert: EdgeConnectionsDropped
        expr: edgeflow_cloudcore_active_connections == 0
        for: 5m
        labels: { severity: critical, component: cloudhub }
        annotations:
          summary: "CloudHub 活跃连接为 0，全部边缘节点断连"
          runbook: "rollback-runbook-v011.md#3-①"

      - alert: RegisterSuccessRateLow
        expr: |
          # 注册成功率（对账口径）：以 load-test 导出或 API 审计为准时替代；
          # 本规则用 5xx 增长作为代理信号
          increase(edgeflow_cloudcore_http_requests_total{code=~"5.."}[5m]) > 10
        for: 5m
        labels: { severity: critical, component: api }
        annotations:
          summary: "cloudcore API 5xx 增多（注册/下发链路异常信号）"
          runbook: "rollback-runbook-v011.md#1"

      # ---- OCSP 限流（warning，S1 对应）----
      - alert: OCSPRateLimited
        expr: increase(edgeflow_cloudcore_http_requests_total{path="/ocsp",code="429"}[5m]) > 0
        for: 5m
        labels: { severity: warning, component: ocsp }
        annotations:
          summary: "/ocsp 出现 429 限流（可能为滥用/攻击或客户端风暴）"
          runbook: "monitoring-alerting-v011.md#6-ocsp"

      # ---- HTTP 错误面（warning）----
      - alert: OCSPErrorsHigh
        expr: increase(edgeflow_cloudcore_http_requests_total{path="/ocsp",code=~"4..|5.."}[15m]) > 20
        for: 15m
        labels: { severity: warning, component: ocsp }
        annotations:
          summary: "/ocsp 非 200 应答增多（吊销查询链路异常）"

      - alert: HTTP5xxRate
        expr: rate(edgeflow_cloudcore_http_requests_total{code=~"5.."}[15m]) * 100 > 1
        for: 15m
        labels: { severity: warning, component: api }
        annotations:
          summary: "API 5xx 比例 >1%（15 分钟窗口）"

      # ---- 节点离线（warning；离线数探测见 §2.5）----
      - alert: NodeOfflineHigh
        expr: edgeflow_cloudcore_nodes_offline_total > 0    # 待该 gauge 落地后启用
        for: 10m
        labels: { severity: warning, component: nodecontroller }
        annotations:
          summary: "存在离线节点超过 10 分钟（NodeController 判定窗口 180s）"
          runbook: "rollback-runbook-v011.md#1"

      # ---- 证书与 CRL（证书过期：critical；临近过期：warning）----
      - alert: CertExpiringSoon
        expr: edgeflow_cert_days_remaining{type="leaf"} < 30    # 由巡检导出（§2.3）
        for: 1h
        labels: { severity: warning, component: certs }
        annotations:
          summary: "云/边证书 30 天内过期（叶子证书有效期 1 年）"
          runbook: "docs/SECURITY.md#3-3（人工轮换：keadm cert rotate）"

      - alert: CertExpiringCritical
        expr: edgeflow_cert_days_remaining{type="leaf"} < 7
        for: 1h
        labels: { severity: critical, component: certs }
        annotations:
          summary: "云/边证书 7 天内过期——mTLS 握手即将全断，立即轮换"

      - alert: CRLNextUpdateExpired
        expr: edgeflow_crl_nextupdate_epoch_seconds < time()
        for: 10m
        labels: { severity: critical, component: certs }
        annotations:
          summary: "crl.pem 已过 nextUpdate——吊销状态不可信"
          runbook: "keadm cert revoke 重新签发 crl.pem（docs/KEADM.md）"

      # ---- 心跳与延迟（基线对照，PERFORMANCE-BASELINE.md）----
      - alert: HeartbeatGapHigh
        expr: |
          # 心跳丢失代理信号：连接数骤降（云边心跳 30s、CloudHub 失活 90s）
          (edgeflow_cloudcore_active_connections
            / on() (edgeflow_cloudcore_active_connections offset 10m)) < 0.5
        for: 5m
        labels: { severity: warning, component: cloudhub }
        annotations:
          summary: "活跃连接 10 分钟内骤降 >50%（疑似节点批量掉线/网络分区）"
          runbook: "rollback-runbook-v011.md#1"
```

> 阈值依据：心跳 30s / CloudHub 失活 90s / NodeController 180s（ARCHITECTURE.md §4.1）；
> 注册成功率基线 100%、N=10 注册 P99 2.28ms（PERFORMANCE-BASELINE.md）；
> 证书有效期 1 年（SECURITY.md §2）。观察期（release-prep-v011.md §5④）按实测校准 `for` 与阈值。

---

## 4. 告警通知矩阵（severity × 角色）

| severity | 研发值班 | 运维值班 | 安全负责人 | 发布审批人 | 通道 |
|----------|---------|---------|-----------|-----------|------|
| critical | ✅ 立即 | ✅ 立即 | ✅（certs 类） | ⏰ 30min 汇总 | 电话/IM 直呼 + 群通知 |
| warning | ✅ 即时 | ✅ 即时 | ⏳ 日报 | — | IM 群通知 |
| info | — | ⏳ 日报 | — | — | 日报邮件 |

- 角色姓名/联系方式沿用 docs/DRILL-SCHEDULE.md §3 角色表（当前【需确认】，发布前回填）。
- **告警风暴熔断**：单规则 5 分钟内重复触发 → Alertmanager `repeat_interval: 1h` 收敛；
  若 5 分钟内 ≥3 条 **critical** 同时激活 → 视为「告警风暴」= 回滚触发条件
  （rollback-runbook-v011.md §1 条件 D），值班立即启动回滚评估，不必等人工确认。

```yaml
# alertmanager.yml 路由片段（草案）
route:
  group_by: ['alertname', 'severity']
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 1h
  receiver: oncall-dev
  routes:
    - matchers: [ 'severity="critical"' ]
      receiver: oncall-dev
    - matchers: [ 'component="certs"' ]
      receiver: security-owner
```

---

## 5. 接入步骤（预发 staging 先行）

1. **Prometheus**：按 §1 配置 scrape（预发本机 `127.0.0.1:8080`，生产 Service DNS）。
2. **告警规则**：§3 文件放入 Prometheus 规则目录，`promtool check rules edgeflow-v011.yml` 校验。
3. **证书/CRL 巡检**（§2.3/2.4，cron 或 textfile collector，每 1h）：

```bash
#!/bin/bash
# hack/cert-watch.sh（草案）：导出证书剩余天数与 crl nextUpdate 到 node_exporter textfile 目录
TEXTFILE=/var/lib/node_exporter/textfile_collector   # 按实际环境调整
CERT_DIR="${EDGEFLOW_CLOUDCORE_CERT_DIR:-data/certs}"
for t in cloudcore edgecore; do
  days=$(echo "($(date -j -f "%b %e %T %Y %Z" \
    "$(openssl x509 -in "$CERT_DIR/$t.crt" -enddate -noout | cut -d= -f2)" "+%s") - $(date +%s)) / 86400" | bc)
  echo "edgeflow_cert_days_remaining{type=\"leaf\",cert=\"$t\"} $days" > "$TEXTFILE/edgeflow_cert.prom"
done
crl_epoch=$(date -j -f "%b %e %T %Y %Z" \
  "$(openssl crl -in "$CERT_DIR/crl.pem" -nextupdate -noout | cut -d= -f2)" "+%s" 2>/dev/null || echo 0)
echo "edgeflow_crl_nextupdate_epoch_seconds $crl_epoch" >> "$TEXTFILE/edgeflow_cert.prom"
```

4. **CRL 降级日志告警（§2.2）**：cloudcore 日志流（kubectl logs / docker 日志驱动）接入 loki，
  规则：`{app="cloudcore"} |= "CRL" |= "Warn"`（或按 S3 实际日志文案匹配）→ warning。
5. **观察期校准**：灰度期间（release-prep-v011.md §5④）确认无噪、无漏，再随全量启用。

---

## 6. 告警处置速查

| 告警 | 第一动作 |
|------|---------|
| CloudCoreDown | rollback-runbook-v011.md：先恢复服务再查根因 |
| EdgeConnectionsDropped | 检查 CloudHub 10000 端口/mTLS 证书/网络；必要时回滚 |
| OCSPRateLimited / OCSPErrorsHigh | 核对限流阈值与客户端行为；疑似攻击→防火墙限源 IP；必要时回滚 S1 修复 |
| CertExpiring* | `keadm cert rotate`（备份先行，SECURITY.md §3.3） |
| CRLNextUpdateExpired | `keadm cert revoke` 重签 crl.pem 或轮换流程 |
| NodeOfflineHigh / HeartbeatGapHigh | 对照 rollback-runbook-v011.md §1 触发条件评估回滚 |

---

*本文件为 v0.1.1 发布保障文件之二；配套 release-prep-v011.md、rollback-runbook-v011.md。*
