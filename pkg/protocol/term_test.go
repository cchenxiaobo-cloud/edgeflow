package protocol

import (
	"strings"
	"testing"
)

// TestV0490TermExecValidate 复核 P3-2：TermExecPayload.Validate 协议边界直测。
func TestV0490TermExecValidate(t *testing.T) {
	valid := TermExecPayload{
		ExecID: "te-1", SessionID: "ts-1", Cmd: "uname", Args: []string{"-a"},
		TimeoutSec: 10, Operator: "op-1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法负载应通过: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*TermExecPayload)
	}{
		{"空 execId", func(p *TermExecPayload) { p.ExecID = "" }},
		{"超长 execId", func(p *TermExecPayload) { p.ExecID = strings.Repeat("x", 65) }},
		{"空 sessionId", func(p *TermExecPayload) { p.SessionID = "" }},
		{"空 cmd", func(p *TermExecPayload) { p.Cmd = "" }},
		{"cmd 含路径分隔符", func(p *TermExecPayload) { p.Cmd = "/bin/sh" }},
		{"cmd 含反斜杠", func(p *TermExecPayload) { p.Cmd = `a\b` }},
		{"cmd 含 NUL", func(p *TermExecPayload) { p.Cmd = "a\x00b" }},
		{"args 超个数", func(p *TermExecPayload) {
			p.Args = make([]string, TermMaxArgs+1)
		}},
		{"args 单参超限", func(p *TermExecPayload) {
			p.Args = []string{strings.Repeat("x", TermMaxArgBytes+1)}
		}},
		{"args 含 NUL", func(p *TermExecPayload) { p.Args = []string{"a\x00"} }},
		{"timeout 越上界", func(p *TermExecPayload) { p.TimeoutSec = TermMaxTimeoutSec + 1 }},
		{"timeout 越下界", func(p *TermExecPayload) { p.TimeoutSec = 0 }},
		{"operator 超限", func(p *TermExecPayload) { p.Operator = strings.Repeat("x", 65) }},
	}
	for _, tc := range cases {
		p := valid
		tc.mut(&p)
		if err := p.Validate(); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}
	// args 总长边界：16 个 1KB 参数（16KB 总限内）通过；超总限拒绝。
	p := valid
	p.Args = make([]string, TermMaxArgs)
	for i := range p.Args {
		p.Args[i] = strings.Repeat("a", TermMaxArgBytes) // 16×4KB=64KB > 16KB 总限
	}
	if err := p.Validate(); err == nil {
		t.Fatal("args 总长超限应拒绝")
	}
}

// TestV0490TermResultValidate 复核 P3-2：TermResultPayload.Validate 协议边界直测。
func TestV0490TermResultValidate(t *testing.T) {
	valid := TermResultPayload{
		ExecID: "te-1", SessionID: "ts-1", OK: true, ExitCode: 0,
		Output: "out", DurationMs: 5, Ts: 1000,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法负载应通过: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*TermResultPayload)
	}{
		{"空 execId", func(p *TermResultPayload) { p.ExecID = "" }},
		{"超长 execId", func(p *TermResultPayload) { p.ExecID = strings.Repeat("x", 65) }},
		{"空 sessionId", func(p *TermResultPayload) { p.SessionID = "" }},
		{"output 超限", func(p *TermResultPayload) { p.Output = strings.Repeat("x", TermMaxOutputBytes+1) }},
		{"reason 超限", func(p *TermResultPayload) { p.Reason = strings.Repeat("x", 513) }},
	}
	for _, tc := range cases {
		p := valid
		tc.mut(&p)
		if err := p.Validate(); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}
	// output 上限内（96KB）通过。
	p := valid
	p.Output = strings.Repeat("x", TermMaxOutputBytes)
	if err := p.Validate(); err != nil {
		t.Fatalf("96KB output 应通过: %v", err)
	}
}
