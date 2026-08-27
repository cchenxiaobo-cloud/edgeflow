package main

// opcua-browse CLI（v0.15.0）：浏览 OPC-UA 服务器两级目录并打印可采集点位。
//
// 用法：go run ./hack/opcua-browse [endpoint]
//
// 输出的 "name=nodeId" 行可直接粘进 EDGEFLOW_OPCUA_NODES。

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	opcuamapper "edgeflow/mappers/opcua"
	"edgeflow/pkg/opcua"
)

func main() {
	endpoint := flag.String("endpoint", "opc.tcp://127.0.0.1:14840", "OPC-UA 服务器端点")
	timeout := flag.Duration("timeout", 5*time.Second, "超时")
	flag.Parse()
	if ep := os.Getenv("OPCUA_ENDPOINT"); ep != "" {
		*endpoint = ep
	}

	cli, err := opcua.Open(*endpoint, *timeout)
	if err != nil {
		fatal("连接失败: %v", err)
	}
	defer func() { _ = cli.Close() }()

	fmt.Printf("# 已连接 %s\n", *endpoint)
	for _, level := range []struct {
		label string
		node  opcua.NodeId
	}{
		{"Objects", opcua.NewNodeID(0, 85)},
	} {
		nodes, err := cli.Browse(level.node)
		if err != nil {
			fatal("浏览 %s 失败: %v", level.label, err)
		}
		for _, n := range nodes {
			if n.NodeClass != 1 { // 只要 Object
				continue
			}
			fmt.Printf("# 对象: %s (ns=%d;i=%d)\n", n.Name, ns(n.NodeId), n.NodeId.Numeric)
			vars, err := cli.Browse(n.NodeId)
			if err != nil {
				continue
			}
			var lines []string
			for _, v := range vars {
				if v.NodeClass != 2 { // 只要 Variable
					continue
				}
				lines = append(lines, fmt.Sprintf("%s=ns=%d;i=%d", v.Name, ns(v.NodeId), v.NodeId.Numeric))
			}
			fmt.Println(strings.Join(lines, ","))
			// 顺带验证 ParseNodes 能解析自家输出
			if _, err := opcuamapper.ParseNodes(strings.Join(lines, ",")); err != nil {
				fmt.Printf("# 警告: ParseNodes 解析失败: %v\n", err)
			}
		}
	}
	fmt.Println("# 完成。将上行 name=nodeId 行粘进 EDGEFLOW_OPCUA_NODES 即可接入。")
	_ = context.Background
}

func ns(n opcua.NodeId) uint32 { return n.Namespace }

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "opcua-browse: "+format+"\n", args...)
	os.Exit(1)
}
