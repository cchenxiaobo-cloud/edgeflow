// cloudcore 是 EdgeFlow 的云端组件入口。
//
// 对标 KubeEdge 的 CloudCore：未来将在这里承载云边通信
// （WebSocket）、消息路由（NATS）、设备管理（CRD）等云端逻辑。
// 当前版本提供：加载配置、启动 HTTP 服务（/healthz 健康检查）、
// 启动 CloudHub（云边 WebSocket 通道，默认端口 10000）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/config"
	"edgeflow/pkg/httpx"
	"edgeflow/pkg/log"
	"edgeflow/pkg/version"
)

func main() {
	// run 返回进程退出码：非 0 表示启动/运行失败
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// run 是 main 的可测试入口：解析命令行 → 加载配置 → 启动服务，
// 返回进程退出码（0 成功，1 失败）。
func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		// 写入 stderr 失败（如管道关闭）时无需额外处理，忽略即可
		_, _ = fmt.Fprintf(stderr, "参数解析失败: %v\n", err)
		return 1
	}

	// --version 打印版本信息后退出
	if opts.version {
		// 写入 stdout 失败时无需额外处理，忽略即可
		_, _ = fmt.Fprintln(stdout, version.Get().String())
		return 0
	}

	// 加载配置：--port > 环境变量 EDGEFLOW_CLOUDCORE_PORT > 配置文件 > 默认值
	cfg, err := config.Load(opts.config, opts.port, opts.portSet)
	if err != nil {
		log.Errorf("配置加载失败: %v", err)
		return 1
	}

	// 打印启动信息与生效配置（含端口来源，便于排查）
	info := version.Get()
	log.Infof("cloudcore starting, %s", info.String())
	log.Infof("生效配置: 端口 %d（来源: %s）", cfg.Port, cfg.PortSource)

	// 解析 CloudHub 端口：环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT > 默认 10000
	hubPort, err := cloudhub.PortFromEnv()
	if err != nil {
		log.Errorf("CloudHub 端口配置无效: %v", err)
		return 1
	}
	hub := cloudhub.New(fmt.Sprintf(":%d", hubPort))

	// 节点注册表（内存态）与 CloudHub 事件桥接：
	// 节点注册/心跳/断开时实时维护节点元数据，供查询 API 使用。
	// 依赖注入（SetNodeEvents），CloudHub 不感知注册表实现。
	nodeReg := registry.New()
	hub.SetNodeEvents(registry.NewCloudHubAdapter(nodeReg))

	// 注册路由：/healthz 健康检查 + /api/v1/nodes 节点查询 API
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", httpx.Healthz())
	api := &nodeAPI{reg: nodeReg}
	mux.HandleFunc("GET /api/v1/nodes", api.listNodes)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", api.getNode)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,  // 防止慢速连接长时间占用连接
		ReadTimeout:       10 * time.Second, // 防止慢速读取长时间占用连接
	}
	return serve(srv, hub)
}

// options 是命令行参数解析结果。
type options struct {
	// port 是 --port 的值（仅当 portSet 为 true 时生效）。
	port int
	// portSet 表示 --port 是否被显式指定。
	portSet bool
	// config 是 --config 指定的配置文件路径。
	config string
	// version 表示是否只打印版本信息后退出。
	version bool
}

// parseFlags 解析命令行参数（使用独立的 FlagSet，便于单元测试）。
func parseFlags(args []string, out io.Writer) (*options, error) {
	fs := flag.NewFlagSet("cloudcore", flag.ContinueOnError)
	fs.SetOutput(out)
	opts := &options{}
	fs.IntVar(&opts.port, "port", 0, "HTTP 服务监听端口（优先级高于环境变量与配置文件；不指定时回落到环境变量/配置文件/默认 8080）")
	fs.StringVar(&opts.config, "config", config.DefaultPath, "配置文件路径（JSON 格式，默认 config/cloudcore.json）")
	fs.BoolVar(&opts.version, "version", false, "打印版本信息后退出")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// 记录 --port 是否被显式设置（flag.Visit 只遍历被设置的 flag）
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			opts.portSet = true
		}
	})
	return opts, nil
}

// serve 启动 HTTP 服务与 CloudHub，等待退出信号后一并优雅关闭，返回进程退出码。
func serve(srv *http.Server, hub *cloudhub.Server) int {
	// 在独立 goroutine 中启动 HTTP 服务与 CloudHub，错误通过 channel 上报主流程
	errCh := make(chan error, 2)
	go func() {
		log.Infof("HTTP server listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		if err := hub.Start(); err != nil {
			errCh <- err
		}
	}()

	// 监听 SIGINT（Ctrl+C）/ SIGTERM（kill）信号，用于优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		// 任一服务启动/运行失败：尽力关闭两者后返回失败
		log.Errorf("服务异常退出: %v", err)
		shutdownAll(srv, hub)
		return 1
	case sig := <-quit:
		log.Infof("收到信号 %s，开始优雅退出...", sig)
	}

	// 给正在处理的请求最多 5 秒完成时间
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok := true
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP 服务优雅退出失败: %v", err)
		ok = false
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Errorf("CloudHub 优雅退出失败: %v", err)
		ok = false
	}
	if !ok {
		return 1
	}
	log.Infof("cloudcore exited")
	return 0
}

// shutdownAll 在服务异常退出时尽力关闭 HTTP 服务与 CloudHub（结果只记日志）。
func shutdownAll(srv *http.Server, hub *cloudhub.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("HTTP 服务关闭失败: %v", err)
	}
	if err := hub.Shutdown(ctx); err != nil {
		log.Errorf("CloudHub 关闭失败: %v", err)
	}
}

// nodeAPI 是节点查询 API（/api/v1/nodes）的处理器集合。
// 通过结构体字段注入注册表（依赖注入，避免全局变量）。
type nodeAPI struct {
	// reg 是节点注册表（与 CloudHub 事件桥接共享同一实例）。
	reg *registry.Registry
}

// listNodes 处理 GET /api/v1/nodes：返回全部节点（JSON 数组，按 NodeID 排序）。
func (a *nodeAPI) listNodes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	_ = json.NewEncoder(w).Encode(a.reg.List())
}

// getNode 处理 GET /api/v1/nodes/{nodeID}：返回单节点详情；节点不存在时 404。
func (a *nodeAPI) getNode(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	info, ok := a.reg.Get(nodeID)
	if !ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		// 编码失败（如客户端已断开）时无需额外处理，忽略即可
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "node not found", "nodeID": nodeID})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 编码失败（如客户端已断开）时无需额外处理，忽略即可
	_ = json.NewEncoder(w).Encode(info)
}
