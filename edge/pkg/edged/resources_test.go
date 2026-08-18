package edged

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/resource"
)

// ---------- request ≤ limit 校验 ----------

func TestValidateRequestLimit(t *testing.T) {
	tests := []struct {
		name    string
		res     metamanager.ResourceRequirements
		wantErr bool
	}{
		{"全部为空→通过", metamanager.ResourceRequirements{}, false},
		{"request==limit→通过", metamanager.ResourceRequirements{
			CPURequest: "500m", CPULimit: "500m",
			MemoryRequest: "64Mi", MemoryLimit: "64Mi"}, false},
		{"request<limit→通过", metamanager.ResourceRequirements{
			CPURequest: "250m", CPULimit: "1",
			MemoryRequest: "64Mi", MemoryLimit: "128Mi"}, false},
		{"仅 request（无 limit）→通过", metamanager.ResourceRequirements{
			CPURequest: "250m", MemoryRequest: "64Mi"}, false},
		{"仅 limit（无 request）→通过", metamanager.ResourceRequirements{
			CPULimit: "1", MemoryLimit: "128Mi"}, false},
		{"仅 request 非法格式→拒绝（fail-open 修复）", metamanager.ResourceRequirements{
			CPURequest: "abc"}, true},
		{"仅 limit 非法格式→拒绝（fail-open 修复）", metamanager.ResourceRequirements{
			CPULimit: "abc"}, true},
		{"仅 memory request 非法格式→拒绝（fail-open 修复）", metamanager.ResourceRequirements{
			MemoryRequest: "12XB"}, true},
		{"仅 memory limit 非法格式→拒绝（fail-open 修复）", metamanager.ResourceRequirements{
			MemoryLimit: "12XB"}, true},
		{"仅 request 非法值 Inf→拒绝", metamanager.ResourceRequirements{
			CPURequest: "Inf"}, true},
		{"CPU request>limit→拒绝", metamanager.ResourceRequirements{
			CPURequest: "1", CPULimit: "250m"}, true},
		{"Memory request>limit→拒绝", metamanager.ResourceRequirements{
			MemoryRequest: "128Mi", MemoryLimit: "64Mi"}, true},
		{"CPU 非法格式→拒绝", metamanager.ResourceRequirements{
			CPURequest: "abc", CPULimit: "1"}, true},
		{"Memory 非法格式→拒绝", metamanager.ResourceRequirements{
			MemoryRequest: "abc", MemoryLimit: "64Mi"}, true},
		{"跨单位比较：750m > 0.5 →拒绝", metamanager.ResourceRequirements{
			CPURequest: "750m", CPULimit: "0.5"}, true},
		{"跨单位比较：0.5 ≤ 750m →通过", metamanager.ResourceRequirements{
			CPURequest: "0.5", CPULimit: "750m"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequestLimit(tt.res)
			if tt.wantErr && err == nil {
				t.Fatalf("期望报错，实际 nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
		})
	}
}

// ---------- 超卖率校验 ----------

// baseCapacity 是测试用节点容量：4 核 / 8GiB。
func baseCapacity() NodeCapacity {
	return NodeCapacity{CPUMilliCores: 4000, MemoryBytes: 8 << 30}
}

// baseRates 是测试用默认超卖率（150%/150%）。
func baseRates() OvercommitRates {
	return OvercommitRates{CPU: 1.5, Memory: 1.5}
}

func TestCheckOvercommit(t *testing.T) {
	cap := baseCapacity() // CPU 上限 6000m，内存上限 12GiB
	rates := baseRates()

	tests := []struct {
		name        string
		newReq      metamanager.ResourceRequirements
		existingCPU int64
		existingMem int64
		wantErr     bool
	}{
		{"无 request→永远允许", metamanager.ResourceRequirements{}, 10000, 100 << 30, false},
		{"空节点+小请求→允许", metamanager.ResourceRequirements{CPURequest: "250m", MemoryRequest: "64Mi"}, 0, 0, false},
		{"已有+新=上限（6000m）→允许（边界：等于上限）", metamanager.ResourceRequirements{CPURequest: "250m"}, 5750, 0, false},
		{"已有+新=上限+1m→拒绝（边界：超过 1m）", metamanager.ResourceRequirements{CPURequest: "251m"}, 5750, 0, true},
		{"内存=上限→允许", metamanager.ResourceRequirements{MemoryRequest: "64Mi"}, 0, 12<<30 - 64<<20, false},
		{"内存=上限+1B→拒绝", metamanager.ResourceRequirements{MemoryRequest: "65Mi"}, 0, 12<<30 - 64<<20, true},
		{"CPU 超卖拒绝（新请求独立超限）", metamanager.ResourceRequirements{CPURequest: "7000m"}, 0, 0, true},
		{"Memory 超卖拒绝", metamanager.ResourceRequirements{MemoryRequest: "13Gi"}, 0, 0, true},
		{"CPU+Memory 同时超限→双理由", metamanager.ResourceRequirements{CPURequest: "7000m", MemoryRequest: "13Gi"}, 0, 0, true},
		{"已超上限+无 request 新 Pod→允许（不占资源）", metamanager.ResourceRequirements{}, 10000, 100 << 30, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckOvercommit(tt.newReq, tt.existingCPU, tt.existingMem, cap, rates)
			if tt.wantErr && err == nil {
				t.Fatalf("期望拒绝，实际 nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("期望允许，实际报错: %v", err)
			}
			if tt.wantErr {
				if !strings.HasPrefix(err.Error(), resource.ErrResourceExhausted) {
					t.Errorf("拒绝错误缺少 %s 标记: %v", resource.ErrResourceExhausted, err)
				}
				if !strings.Contains(err.Error(), "超出节点资源") {
					t.Errorf("拒绝错误缺少「超出节点资源」语义: %v", err)
				}
			}
		})
	}
}

func TestCheckOvercommitCPUMemoryIndependent(t *testing.T) {
	cap := baseCapacity()
	rates := baseRates()

	// 只有 CPU 超限 → 报 CPU 理由，不报 Memory
	err := CheckOvercommit(metamanager.ResourceRequirements{CPURequest: "7000m"}, 0, 0, cap, rates)
	if err == nil {
		t.Fatal("CPU 超限应拒绝")
	}
	if !strings.Contains(err.Error(), "CPU") {
		t.Errorf("错误应包含 CPU 理由: %v", err)
	}
	if strings.Contains(err.Error(), "Memory") {
		t.Errorf("Memory 未超限不应出现在理由中: %v", err)
	}

	// 只有 Memory 超限
	err = CheckOvercommit(metamanager.ResourceRequirements{MemoryRequest: "13Gi"}, 0, 0, cap, rates)
	if err == nil {
		t.Fatal("Memory 超限应拒绝")
	}
	if !strings.Contains(err.Error(), "Memory") {
		t.Errorf("错误应包含 Memory 理由: %v", err)
	}
	if strings.Contains(err.Error(), "CPU") {
		t.Errorf("CPU 未超限不应出现在理由中: %v", err)
	}
}

func TestCheckOvercommitRateConfigurable(t *testing.T) {
	cap := baseCapacity()

	// 超卖率 100%（不允许超卖）：5000m > 4000m 上限 → 拒绝
	err := CheckOvercommit(metamanager.ResourceRequirements{CPURequest: "5000m"}, 0, 0, cap, OvercommitRates{CPU: 1.0, Memory: 1.0})
	if err == nil {
		t.Error("超卖率 100% 时 5000m 应拒绝（节点只有 4000m）")
	}
	// 超卖率 200%：5000m ≤ 8000m 上限 → 允许
	err = CheckOvercommit(metamanager.ResourceRequirements{CPURequest: "5000m"}, 0, 0, cap, OvercommitRates{CPU: 2.0, Memory: 2.0})
	if err != nil {
		t.Errorf("超卖率 200%% 时 5000m 应允许: %v", err)
	}
}

// ---------- 请求求和（含副本） ----------

func TestSumPodRequests(t *testing.T) {
	pods := []metamanager.Pod{
		{Name: "a", Replicas: 2, Resources: metamanager.ResourceRequirements{CPURequest: "250m", MemoryRequest: "64Mi"}},
		{Name: "b", Replicas: 3, Resources: metamanager.ResourceRequirements{CPURequest: "100m"}},
		{Name: "c", Replicas: 0, Resources: metamanager.ResourceRequirements{MemoryRequest: "128Mi"}}, // replicas 缺省按 1
		{Name: "d", Resources: metamanager.ResourceRequirements{}},                                    // 无 request
		{Name: "e", Resources: metamanager.ResourceRequirements{CPURequest: "非法"}},                    // 脏数据按 0
	}
	cpu, mem := SumPodRequests(pods)
	wantCPU := int64(2*250 + 3*100 + 1*0 + 0 + 0)
	wantMem := int64(2*(64<<20) + 0 + 128<<20 + 0 + 0)
	if cpu != wantCPU {
		t.Errorf("CPU 总和 = %d，期望 %d", cpu, wantCPU)
	}
	if mem != wantMem {
		t.Errorf("内存总和 = %d，期望 %d", mem, wantMem)
	}
}

// ---------- docker 参数生成 ----------

func TestDockerResourceArgs(t *testing.T) {
	tests := []struct {
		name    string
		res     metamanager.ResourceRequirements
		want    []string
		wantErr bool
	}{
		{"全部为空→无参数", metamanager.ResourceRequirements{}, nil, false},
		{"仅 CPU limit", metamanager.ResourceRequirements{CPULimit: "250m"},
			[]string{"--cpus=0.25"}, false},
		{"整核 CPU", metamanager.ResourceRequirements{CPULimit: "2"},
			[]string{"--cpus=2"}, false},
		{"分数核", metamanager.ResourceRequirements{CPULimit: "1.5"},
			[]string{"--cpus=1.5"}, false},
		{"仅 Memory limit", metamanager.ResourceRequirements{MemoryLimit: "64Mi"},
			[]string{"--memory=67108864", "--memory-swap=67108864"}, false},
		{"CPU+Memory 都有", metamanager.ResourceRequirements{CPULimit: "500m", MemoryLimit: "1Gi"},
			[]string{"--cpus=0.5", "--memory=1073741824", "--memory-swap=1073741824"}, false},
		{"仅 request 无 limit→无参数", metamanager.ResourceRequirements{CPURequest: "250m", MemoryRequest: "64Mi"},
			nil, false},
		{"非法 CPU limit→报错", metamanager.ResourceRequirements{CPULimit: "abc"}, nil, true},
		{"非法 Memory limit→报错", metamanager.ResourceRequirements{MemoryLimit: "abc"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DockerResourceArgs(tt.res)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际 nil（args=%v）", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("参数 = %v，期望 %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("参数[%d] = %q，期望 %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ---------- 超卖拒绝错误消息文本 ----------

func TestCheckOvercommitErrorMessageContent(t *testing.T) {
	err := CheckOvercommit(metamanager.ResourceRequirements{CPURequest: "250m"}, 5800, 0, baseCapacity(), baseRates())
	if err == nil {
		t.Fatal("应拒绝")
	}
	msg := err.Error()
	for _, want := range []string{resource.ErrResourceExhausted, "超出节点资源", "CPU", "250m", "5800m", "6050m", "6000m", "4000m", "150"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误消息缺少 %q: %s", want, msg)
		}
	}
}

// ---------- DockerRuntime 集成：EnsureRunning 时传递资源参数 ----------

// TestDockerEnsureRunningResourceArgs 验证：Pod 带资源 limit 时，
// docker run 参数包含 --cpus / --memory / --memory-swap；无资源时不含。
func TestDockerEnsureRunningResourceArgs(t *testing.T) {
	t.Run("带资源 limit", func(t *testing.T) {
		var runCmd string
		d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
			if args[0] == "run" {
				runCmd = strings.Join(args, " ")
				return "abc123\n", nil
			}
			// 状态 inspect：容器不存在
			return "", errFakeNoObject
		})
		pod := podNginx()
		pod.Resources = metamanager.ResourceRequirements{
			CPULimit:      "250m",
			MemoryLimit:   "64Mi",
			CPURequest:    "100m",
			MemoryRequest: "32Mi",
		}
		if err := d.EnsureRunning(pod, 0); err != nil {
			t.Fatalf("EnsureRunning 失败: %v", err)
		}
		for _, want := range []string{
			"--cpus=0.25",
			"--memory=67108864",
			"--memory-swap=67108864",
		} {
			if !strings.Contains(runCmd, want) {
				t.Errorf("docker run 缺少资源参数 %q: %s", want, runCmd)
			}
		}
		// request 不传给 docker（只传 limit）
		if strings.Contains(runCmd, "100m") || strings.Contains(runCmd, "32Mi") {
			t.Errorf("request 不应出现在 docker 参数中: %s", runCmd)
		}
	})

	t.Run("无资源→无资源参数", func(t *testing.T) {
		var runCmd string
		d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
			if args[0] == "run" {
				runCmd = strings.Join(args, " ")
				return "abc123\n", nil
			}
			return "", errFakeNoObject
		})
		if err := d.EnsureRunning(podNginx(), 0); err != nil {
			t.Fatalf("EnsureRunning 失败: %v", err)
		}
		if strings.Contains(runCmd, "--cpus") || strings.Contains(runCmd, "--memory") {
			t.Errorf("无资源 Pod 不应含资源参数: %s", runCmd)
		}
	})

	t.Run("非法 CPU limit→拒绝创建", func(t *testing.T) {
		d := newTestDocker(fakeRunner(
			[]any{"", errFakeNoObject}, // 状态 inspect：不存在
		))
		pod := podNginx()
		pod.Resources = metamanager.ResourceRequirements{CPULimit: "abc"}
		if err := d.EnsureRunning(pod, 0); err == nil {
			t.Fatal("非法 CPU limit 应报错")
		} else if !strings.Contains(err.Error(), "CPU limit 解析失败") {
			t.Errorf("错误应指明 CPU limit 解析失败: %v", err)
		}
	})
}

// TestDockerEnsureRunningResourceDrift 验证 WBS 6.5 资源漂移重建：
// 已运行且镜像一致但资源 limit 不一致 → 先停再重建（带新参数）；
// 资源一致 → no-op；期望不带 limit → 不检查（兼容旧语义）。
func TestDockerEnsureRunningResourceDrift(t *testing.T) {
	t.Run("资源一致→no-op", func(t *testing.T) {
		var calls []string
		d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			switch {
			case args[0] == "inspect" && strings.Contains(strings.Join(args, " "), "Config.Image"):
				return "nginx:1.27\n", nil // 镜像一致
			case args[0] == "inspect" && strings.Contains(strings.Join(args, " "), "HostConfig"):
				return "250000000 67108864\n", nil // 资源一致（250m / 64Mi）
			default:
				return "true\n", nil // 状态：已运行
			}
		})
		pod := podNginx()
		pod.Resources = metamanager.ResourceRequirements{CPULimit: "250m", MemoryLimit: "64Mi"}
		if err := d.EnsureRunning(pod, 0); err != nil {
			t.Fatalf("资源一致 EnsureRunning 失败: %v", err)
		}
		if n := countPrefix(calls, "rm "); n != 0 {
			t.Errorf("资源一致不应 rm，实际 %d 次: %v", n, calls)
		}
		if n := countPrefix(calls, "run "); n != 0 {
			t.Errorf("资源一致不应 run，实际 %d 次: %v", n, calls)
		}
	})

	t.Run("资源不一致→重建（顺序断言）", func(t *testing.T) {
		var calls []string
		d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
			calls = append(calls, args[0])
			switch {
			case len(calls) == 1: // 状态 inspect
				return "true\n", nil
			case len(calls) == 2: // 镜像 inspect
				return "nginx:1.27\n", nil // 镜像一致
			case len(calls) == 3: // HostConfig inspect：旧资源（500m）
				return "500000000 0\n", nil
			case len(calls) == 4: // rm -f
				return "", nil
			case len(calls) == 5: // 重建前状态 inspect
				return "", errFakeNoObject
			default: // run -d（带新资源参数）
				return "abc123\n", nil
			}
		})
		pod := podNginx()
		pod.Resources = metamanager.ResourceRequirements{CPULimit: "250m", MemoryLimit: "64Mi"}
		if err := d.EnsureRunning(pod, 0); err != nil {
			t.Fatalf("资源漂移重建失败: %v", err)
		}
		want := []string{"inspect", "inspect", "inspect", "rm", "inspect", "run"}
		if len(calls) != len(want) {
			t.Fatalf("调用次数 = %d，期望 %d: %v", len(calls), len(want), calls)
		}
		for i, w := range want {
			if calls[i] != w {
				t.Errorf("第 %d 次调用 = %q，期望 %q（先停再建）", i+1, calls[i], w)
			}
		}
	})

	t.Run("期望不带 limit→不检查不重建", func(t *testing.T) {
		var calls []string
		d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			if strings.Contains(strings.Join(args, " "), "Config.Image") {
				return "nginx:1.27\n", nil
			}
			return "true\n", nil
		})
		if err := d.EnsureRunning(podNginx(), 0); err != nil {
			t.Fatalf("无 limit EnsureRunning 失败: %v", err)
		}
		for _, c := range calls {
			if strings.Contains(c, "HostConfig") {
				t.Errorf("无 limit 不应做 HostConfig inspect: %v", calls)
			}
		}
	})
}

// ---------- 超卖率环境变量（P0-m5） ----------

// TestDefaultOvercommitRatesEnv 验证：合法环境变量生效，非法值
// （含 strconv.ParseFloat 会静默接受的 Inf/NaN 等非有限值）回退默认。
func TestDefaultOvercommitRatesEnv(t *testing.T) {
	t.Run("未设置→默认 1.5/1.5", func(t *testing.T) {
		r := DefaultOvercommitRates()
		if r.CPU != 1.5 || r.Memory != 1.5 {
			t.Errorf("默认 = %+v，期望 {CPU:1.5 Memory:1.5}", r)
		}
	})

	t.Run("合法值生效", func(t *testing.T) {
		t.Setenv("EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE", "2.5")
		t.Setenv("EDGEFLOW_EDGECORE_OVERCOMMIT_MEMORY_RATE", "1.25")
		r := DefaultOvercommitRates()
		if r.CPU != 2.5 || r.Memory != 1.25 {
			t.Errorf("环境变量覆盖 = %+v，期望 {CPU:2.5 Memory:1.25}", r)
		}
	})

	illegal := []struct {
		name string
		cpu  string
		mem  string
	}{
		{"Inf", "Inf", "Inf"},
		{"+Inf", "+Inf", "+Inf"},
		{"-Inf", "-Inf", "-Inf"},
		{"NaN", "NaN", "NaN"},
		{"非数字", "abc", "12XB"},
		{"非正值", "0", "-1.5"},
	}
	for _, tt := range illegal {
		t.Run("非法值回退默认: "+tt.name, func(t *testing.T) {
			t.Setenv("EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE", tt.cpu)
			t.Setenv("EDGEFLOW_EDGECORE_OVERCOMMIT_MEMORY_RATE", tt.mem)
			r := DefaultOvercommitRates()
			if r.CPU != 1.5 || r.Memory != 1.5 {
				t.Errorf("非法输入应回退默认，实际 %+v", r)
			}
		})
	}
}

// ---------- 节点容量环境变量覆盖 ----------

// TestDetectNodeCapacityEnvOverride 验证：环境变量合法时覆盖探测值。
func TestDetectNodeCapacityEnvOverride(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_NODE_CPU_MILLI", "8000")
	t.Setenv("EDGEFLOW_EDGECORE_NODE_MEMORY_BYTES", "1073741824")
	c := DetectNodeCapacity()
	if c.CPUMilliCores != 8000 {
		t.Errorf("CPUMilliCores = %d，期望 8000（环境变量覆盖）", c.CPUMilliCores)
	}
	if c.MemoryBytes != 1073741824 {
		t.Errorf("MemoryBytes = %d，期望 1073741824（环境变量覆盖）", c.MemoryBytes)
	}
}

// TestDetectNodeCapacityEnvInvalidFallsBack 验证：环境变量非法时回退探测值
// （CPU 回退 runtime.NumCPU×1000，内存回退 detectMemoryBytes 结果）。
func TestDetectNodeCapacityEnvInvalidFallsBack(t *testing.T) {
	t.Setenv("EDGEFLOW_EDGECORE_NODE_CPU_MILLI", "abc")
	t.Setenv("EDGEFLOW_EDGECORE_NODE_MEMORY_BYTES", "-5")
	c := DetectNodeCapacity()
	if want := int64(runtime.NumCPU()) * 1000; c.CPUMilliCores != want {
		t.Errorf("非法 CPU 环境变量应回退探测值 %d，实际 %d", want, c.CPUMilliCores)
	}
	if want := detectMemoryBytes(); c.MemoryBytes != want {
		t.Errorf("非法内存环境变量应回退探测值 %d，实际 %d", want, c.MemoryBytes)
	}
}
