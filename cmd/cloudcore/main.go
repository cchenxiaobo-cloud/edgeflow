// cloudcore 是 EdgeFlow 的云端组件入口。
//
// 对标 KubeEdge 的 CloudCore：未来将在这里承载云边通信
// （WebSocket）、消息路由（NATS）、设备管理（CRD）等云端逻辑。
// 当前版本提供最小可运行服务：启动 HTTP 服务并暴露 /healthz 健康检查。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"edgeflow/pkg/httpx"
	"edgeflow/pkg/log"
	"edgeflow/pkg/version"
)

// defaultPort 是未指定任何配置时的默认监听端口。
const defaultPort = 8080

// portEnvVar 是用于覆盖监听端口的环境变量名。
const portEnvVar = "EDGEFLOW_CLOUDCORE_PORT"

func main() {
	// 命令行参数：--port 指定监听端口，--version 打印版本信息后退出
	port := flag.Int("port", defaultPort, "HTTP 服务监听端口（默认 8080）")
	showVersion := flag.Bool("version", false, "打印版本信息后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get().String())
		return
	}

	// 端口优先级：命令行参数 > 环境变量 > 默认值
	listenPort := *port
	explicitPort := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			explicitPort = true
		}
	})
	if !explicitPort {
		if env := os.Getenv(portEnvVar); env != "" {
			if v, err := strconv.Atoi(env); err == nil {
				listenPort = v
			} else {
				log.Warnf("环境变量 %s 的值 %q 不是合法端口，忽略", portEnvVar, env)
			}
		}
	}

	// 打印启动信息
	info := version.Get()
	log.Infof("cloudcore starting, %s", info.String())

	// 注册路由：/healthz 健康检查
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", httpx.Healthz())

	addr := fmt.Sprintf(":%d", listenPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // 防止慢速连接长时间占用连接
	}

	// 在独立 goroutine 中启动 HTTP 服务，错误通过 channel 上报主流程
	errCh := make(chan error, 1)
	go func() {
		log.Infof("HTTP server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 监听 SIGINT（Ctrl+C）/ SIGTERM（kill）信号，用于优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Errorf("HTTP server 异常退出: %v", err)
		os.Exit(1)
	case sig := <-quit:
		log.Infof("收到信号 %s，开始优雅退出...", sig)
	}

	// 给正在处理的请求最多 5 秒完成时间
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("优雅退出失败: %v", err)
		os.Exit(1)
	}
	log.Infof("cloudcore exited")
}
