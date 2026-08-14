package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/template"
)

// jsonMarshalIndent 是 version --json 的序列化辅助（独立出来便于测试/替换）。
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// initOptions 是 keadm init 的参数集合。
type initOptions struct {
	// Image 是 cloudcore 容器镜像（默认 edgeflow/cloudcore:v0.1.0，与 Chart 默认一致）。
	Image string
	// TLS 是否启用云边 mTLS（透传 EDGEFLOW_CLOUDCORE_TLS=on + CERT_DIR=/data/certs）。
	TLS bool
	// TLSSAN 是服务端证书 SAN（逗号分隔，如 IP:1.2.3.4,DNS:edgeflow.example.com），
	// 透传 EDGEFLOW_CLOUDCORE_TLS_SAN；为空时不注入（cloudcore 使用默认 SAN）。
	TLSSAN string
	// ServiceType 是生成的 Service 类型（NodePort 默认，边缘节点跨集群接入；
	// 仅集群内访问可改 ClusterIP）。
	ServiceType string
	// OutputDir 是产物输出目录。
	OutputDir string
}

// initOutputs 是 keadm init 生成的产物清单（reset 依据此清单清理）。
var initOutputs = []string{"cloudcore.yaml", "NOTES.txt"}

// runInit 实现 keadm init：生成云端部署产物。
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("init", stderr)
	opts := initOptions{
		Image:       "edgeflow/cloudcore:v0.1.0",
		ServiceType: "NodePort",
		OutputDir:   "./keadm-out",
	}
	fs.StringVar(&opts.Image, "cloudcore-image", opts.Image, "cloudcore 容器镜像（默认 edgeflow/cloudcore:v0.1.0）")
	fs.BoolVar(&opts.TLS, "tls", false, "启用云边 mTLS（注入 EDGEFLOW_CLOUDCORE_TLS=on）")
	fs.StringVar(&opts.TLSSAN, "tls-san", "", "cloudcore 证书 SAN（逗号分隔，如 IP:1.2.3.4；仅 --tls 时生效）")
	fs.StringVar(&opts.ServiceType, "service-type", opts.ServiceType, "Service 类型：NodePort（默认，边缘跨集群接入）或 ClusterIP")
	fs.StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "产物输出目录")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: init 不接受位置参数 %q\n", fs.Arg(0))
		return exitUsage
	}

	// 参数校验：镜像与 Service 类型非空，Service 类型取值合法。
	if opts.Image == "" {
		fmt.Fprintln(stderr, "错误: --cloudcore-image 不能为空")
		return exitUsage
	}
	if opts.ServiceType != "NodePort" && opts.ServiceType != "ClusterIP" {
		fmt.Fprintf(stderr, "错误: --service-type 仅支持 NodePort 或 ClusterIP，当前 %q\n", opts.ServiceType)
		return exitUsage
	}

	// 创建输出目录（已存在则复用，保证重复执行幂等）。
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "错误: 创建输出目录 %s 失败: %v\n", opts.OutputDir, err)
		return exitRuntime
	}

	// 生成 cloudcore.yaml（Deployment + Service，可直接 kubectl apply -f）。
	yamlPath := filepath.Join(opts.OutputDir, "cloudcore.yaml")
	yamlBytes, err := renderCloudcoreYAML(opts)
	if err != nil {
		fmt.Fprintf(stderr, "错误: 生成 cloudcore.yaml 失败: %v\n", err)
		return exitRuntime
	}
	if err := os.WriteFile(yamlPath, yamlBytes, 0o644); err != nil {
		fmt.Fprintf(stderr, "错误: 写入 %s 失败: %v\n", yamlPath, err)
		return exitRuntime
	}

	// 生成 NOTES.txt（部署说明 + Helm 替代路径）。
	notesPath := filepath.Join(opts.OutputDir, "NOTES.txt")
	notesBytes := renderInitNotes(opts)
	if err := os.WriteFile(notesPath, notesBytes, 0o644); err != nil {
		fmt.Fprintf(stderr, "错误: 写入 %s 失败: %v\n", notesPath, err)
		return exitRuntime
	}

	fmt.Fprintf(stdout, "keadm init 完成: 云端部署产物已生成到 %s\n", opts.OutputDir)
	fmt.Fprintf(stdout, "  - %s（kubectl apply -f 即可部署）\n", yamlPath)
	fmt.Fprintf(stdout, "  - %s（部署说明与 Helm 替代路径）\n", notesPath)
	if opts.TLS {
		fmt.Fprintln(stdout, "提示: 已启用 mTLS（cloudcore 首次启动会在 /data/certs 自动生成证书）。")
		fmt.Fprintln(stdout, "      边缘节点接入请确保证书 SAN 覆盖访问地址（--tls-san 或部署后手动注入）。")
	}
	return exitOK
}

// cloudcoreYAML 模板与 Chart（build/charts/edgeflow）约定对齐：
//   - 镜像、端口（http 8080 / hub 10000）、/data 卷、/healthz 探针、
//     TLS env 名（EDGEFLOW_CLOUDCORE_TLS / EDGEFLOW_CLOUDCORE_CERT_DIR）全部一致；
//   - 安全上下文与 Chart values 一致（nonroot 65532 + 只读根文件系统 + /data 可写）。
var cloudcoreYAMLTemplate = template.Must(template.New("cloudcore").Parse(`# 由 keadm init 生成（EdgeFlow WBS 8.6）。可直接 kubectl apply -f 部署。
# 与 build/charts/edgeflow 的容器约定一致：/healthz 探针、/data 卷、TLS env 透传。
apiVersion: v1
kind: Service
metadata:
  name: edgeflow-cloudcore
  labels:
    app.kubernetes.io/name: edgeflow
    app.kubernetes.io/component: cloudcore
spec:
  # NodePort：边缘节点在集群外，需经节点端口接入 CloudHub；仅集群内访问可改 ClusterIP
  type: {{ .ServiceType }}
  selector:
    app.kubernetes.io/name: edgeflow
    app.kubernetes.io/component: cloudcore
  ports:
    - name: http
      port: 8080
      targetPort: http
      protocol: TCP
    - name: hub
      port: 10000
      targetPort: hub
      protocol: TCP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: edgeflow-cloudcore
  labels:
    app.kubernetes.io/name: edgeflow
    app.kubernetes.io/component: cloudcore
spec:
  # 云边通信为有状态长连接，副本固定 1（与 Chart 默认一致）
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: edgeflow
      app.kubernetes.io/component: cloudcore
  template:
    metadata:
      labels:
        app.kubernetes.io/name: edgeflow
        app.kubernetes.io/component: cloudcore
    spec:
      # 优雅退出宽限期：SIGTERM 后排空存量云边连接
      terminationGracePeriodSeconds: 30
      # 与 distroless nonroot 运行镜像匹配（uid 65532）
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
      volumes:
        # /data 可写卷：mTLS 证书自动生成落在 /data/certs/
        - name: data
          emptyDir: {}
      containers:
        - name: cloudcore
          image: {{ .Image }}
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
            - name: hub
              containerPort: 10000
              protocol: TCP
          volumeMounts:
            - name: data
              mountPath: /data
          env:
            - name: EDGEFLOW_CLOUDCORE_PORT
              value: "8080"
            - name: EDGEFLOW_CLOUDCORE_HUB_PORT
              value: "10000"
{{- if .TLS }}
            # mTLS：首次启动在 /data/certs 自动生成 CA 与服务端证书（幂等）
            - name: EDGEFLOW_CLOUDCORE_TLS
              value: "on"
            - name: EDGEFLOW_CLOUDCORE_CERT_DIR
              value: "/data/certs"
{{- end }}
{{- if .TLSSAN }}
            # 证书 SAN：边缘节点用 IP 接入时必须覆盖其访问地址
            - name: EDGEFLOW_CLOUDCORE_TLS_SAN
              value: {{ printf "%q" .TLSSAN }}
{{- end }}
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 10
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 5
            timeoutSeconds: 3
            failureThreshold: 3
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 512Mi
`))

// renderCloudcoreYAML 渲染 Deployment+Service YAML 文本。
func renderCloudcoreYAML(opts initOptions) ([]byte, error) {
	var buf bytes.Buffer
	if err := cloudcoreYAMLTemplate.Execute(&buf, opts); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderInitNotes 生成 NOTES.txt：部署步骤、验证方法、Helm 替代路径。
func renderInitNotes(opts initOptions) []byte {
	s := fmt.Sprintf(`EdgeFlow CloudCore 部署说明（由 keadm init 生成）
==============================================

一、部署（任选一种方式）

方式 A：直接应用生成的 YAML（推荐）
  kubectl apply -f cloudcore.yaml
  # 查看状态
  kubectl get deploy,svc,pods -l app.kubernetes.io/component=cloudcore

方式 B：Helm Chart（build/charts/edgeflow，功能等价）
  helm install edgeflow build/charts/edgeflow \
    --set service.hubEnabled=true \
    --set service.type=%s \
    %s
  # 说明：Chart 默认 ClusterIP 且 hub 不暴露；边缘接入需按上例开启。

二、验证

  # 端口转发后访问健康检查
  kubectl port-forward svc/edgeflow-cloudcore 8080:8080
  curl http://127.0.0.1:8080/healthz
  # 期望输出 HTTP 200

三、边缘节点接入

  1. 获取 CloudHub 节点端口（NodePort 自动分配在 30000-32767）：
     kubectl get svc edgeflow-cloudcore -o jsonpath='{.spec.ports[?(@.name=="hub")].nodePort}'
  2. 在边缘节点执行（把 <ip> 换成集群任一节点 IP、<port> 换成上面的端口）：
     keadm join --cloudcore-ip=<ip> --cloudcore-port=<port> --token=<token> %s

四、mTLS 说明%s

五、清理

  kubectl delete -f cloudcore.yaml
`, opts.ServiceType, tlsHelmSnippet(opts), tlsJoinSnippet(opts), tlsNotesSection(opts))
	return []byte(s)
}

// tlsHelmSnippet 生成方式 B 中的 TLS 相关 --set 片段。
func tlsHelmSnippet(opts initOptions) string {
	if !opts.TLS {
		return "# 未启用 mTLS"
	}
	if opts.TLSSAN != "" {
		return fmt.Sprintf("--set cloudcore.env.EDGEFLOW_CLOUDCORE_TLS=on \\\n    --set cloudcore.env.EDGEFLOW_CLOUDCORE_CERT_DIR=/data/certs \\\n    --set cloudcore.env.EDGEFLOW_CLOUDCORE_TLS_SAN=%q", opts.TLSSAN)
	}
	return "--set cloudcore.env.EDGEFLOW_CLOUDCORE_TLS=on \\\n    --set cloudcore.env.EDGEFLOW_CLOUDCORE_CERT_DIR=/data/certs"
}

// tlsJoinSnippet 生成 join 建议命令中的 TLS 片段。
func tlsJoinSnippet(opts initOptions) string {
	if opts.TLS {
		return "--tls"
	}
	return ""
}

// tlsNotesSection 生成 mTLS 章节内容（未启用时给出启用指引）。
func tlsNotesSection(opts initOptions) string {
	if opts.TLS {
		extra := ""
		if opts.TLSSAN != "" {
			extra = fmt.Sprintf("\n  已注入 SAN: %s（需确保覆盖边缘节点访问的 IP/DNS）", opts.TLSSAN)
		}
		return fmt.Sprintf(`
  本次部署已启用 mTLS（EDGEFLOW_CLOUDCORE_TLS=on）：
  cloudcore 首次启动会在 /data/certs 自动生成 CA 与服务端证书（幂等）。
  edgecore 侧使用 keadm join --tls 生成的证书目录完成双向认证。%s
`, extra)
	}
	return `
  本次部署未启用 mTLS（云边通道为明文 ws://）。
  生产环境建议重新执行：keadm init --tls（或 Chart 注入 TLS env）。
`
}
