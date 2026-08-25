// v0.6.0 多副本相关装配单测：
//   - /healthz 三态（etcdAwareHealthz：健康 200 / 失联 503；非多副本形态不绑定）
//   - MULTI_REPLICA env 解析（空/0/false=关；1/true=开；非法 fail-fast）
//   - NODE_LEASE_TTL env 解析边界（默认 300s；显式合法值；非法 fail-fast）
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"edgeflow/cloud/pkg/etcdstore"
)

// fakeHealthChecker：可控的健康检查实现。
type fakeHealthChecker struct{ healthy bool }

func (f *fakeHealthChecker) EtcdHealthyWithin(_ time.Duration) bool { return f.healthy }

// 1) /healthz：etcd 健康 → 200（进程存活语义不变）
func TestEtcdAwareHealthzHealthy(t *testing.T) {
	h := etcdAwareHealthz(&fakeHealthChecker{healthy: true}, 300)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("健康时应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// 2) /healthz：etcd 失联超过 TTL → 503（K8s liveness 重启自愈路径）
func TestEtcdAwareHealthzUnhealthy(t *testing.T) {
	h := etcdAwareHealthz(&fakeHealthChecker{healthy: false}, 300)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("失联应 503，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

// 3) MULTI_REPLICA 解析表驱动
func TestMultiReplicaFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
		ok   bool // false 期望 error
	}{
		{"", false, true},
		{"0", false, true},
		{"false", false, true},
		{"1", true, true},
		{"true", true, true},
		{"TRUE", false, false}, // 大小写敏感（文档口径："1"/"true"）
		{"yes", false, false},
		{"2", false, false},
		{"off", false, false},
	}
	for _, c := range cases {
		t.Setenv(envMultiReplica, c.val)
		got, err := multiReplicaFromEnv()
		if c.ok && err != nil {
			t.Errorf("MULTI_REPLICA=%q: 不应报错: %v", c.val, err)
		}
		if !c.ok && err == nil {
			t.Errorf("MULTI_REPLICA=%q: 应报错，实际 nil", c.val)
		}
		if c.ok && got != c.want {
			t.Errorf("MULTI_REPLICA=%q: got %v want %v", c.val, got, c.want)
		}
	}
}

// 4) NODE_LEASE_TTL 解析边界
func TestLeaseTTLFromEnv(t *testing.T) {
	t.Setenv(etcdstore.EnvLeaseTTL, "")
	if d, err := etcdstore.LeaseTTLFromEnv(); err != nil || d.Seconds() != 300 {
		t.Fatalf("默认应 300s: d=%v err=%v", d, err)
	}
	t.Setenv(etcdstore.EnvLeaseTTL, "5m")
	if d, err := etcdstore.LeaseTTLFromEnv(); err != nil || d.Seconds() != 300 {
		t.Fatalf("'5m' 应 300s: d=%v err=%v", d, err)
	}
	t.Setenv(etcdstore.EnvLeaseTTL, "300")
	if d, err := etcdstore.LeaseTTLFromEnv(); err != nil || d.Seconds() != 300 {
		t.Fatalf("'300'（秒）应 300s: d=%v err=%v", d, err)
	}
	t.Setenv(etcdstore.EnvLeaseTTL, "600s")
	if d, err := etcdstore.LeaseTTLFromEnv(); err != nil || d.Seconds() != 600 {
		t.Fatalf("'600s' 应 600s: d=%v err=%v", d, err)
	}
	t.Setenv(etcdstore.EnvLeaseTTL, "0")
	if _, err := etcdstore.LeaseTTLFromEnv(); err == nil {
		t.Fatal("'0' 应 fail-fast")
	}
	t.Setenv(etcdstore.EnvLeaseTTL, "bogus")
	if _, err := etcdstore.LeaseTTLFromEnv(); err == nil {
		t.Fatal("非法值应 fail-fast")
	}
}