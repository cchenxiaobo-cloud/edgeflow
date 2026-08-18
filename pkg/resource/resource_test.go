package resource

import (
	"strings"
	"testing"
)

func TestParseCPU(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"250m", 250, false},
		{"1m", 1, false},
		{"0m", 0, false},
		{"1000m", 1000, false},
		{"1", 1000, false},
		{"0", 0, false},
		{"0.5", 500, false},
		{"1.5", 1500, false},
		{"2", 2000, false},
		{"0.25", 250, false},
		{"0.001", 1, false},
		{"-1", 0, true},
		{"-250m", 0, true},
		{"1e3", 0, true},
		{"abc", 0, true},
		{"250x", 0, true},
		{"m", 0, true},
		// M1：非有限值与超出 int64 毫核范围必须报错（不得静默转成未定义值）
		{"Inf", 0, true},
		{"+Inf", 0, true},
		{"-Inf", 0, true},
		{"Infinity", 0, true},
		{"NaN", 0, true},
		{"9223372036854775", 0, true}, // float64 解析后 ×1000 ≥ 2^63，超出 int64 毫核
		// n5：前导 + 必须拒绝（strconv.ParseInt/ParseFloat 会静默接受）
		{"+250m", 0, true},
		{"+1", 0, true},
		// 溢出边界毫核字面量
		{"9223372036854775808m", 0, true},
		// 大值但仍在 int64 毫核范围内的合法值不受影响
		{"9000000", 9000000000, false},
	}
	for _, tt := range tests {
		got, err := ParseCPU(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseCPU(%q) 期望报错，实际 nil（got=%d）", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCPU(%q) 意外报错: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseCPU(%q) = %d, 期望 %d", tt.in, got, tt.want)
		}
	}
}

func TestParseCPU_Whitespace(t *testing.T) {
	got, err := ParseCPU(" 250m ")
	if err != nil || got != 250 {
		t.Errorf("ParseCPU(\" 250m \") = (%d, %v), 期望 (250, nil)", got, err)
	}
}

func TestParseMemory(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		// 纯数字 = 字节
		{"0", 0, false},
		{"67108864", 67108864, false},
		// 二进制后缀
		{"64Ki", 64 << 10, false},
		{"64Mi", 64 << 20, false},
		{"1Gi", 1 << 30, false},
		{"2Ti", 2 << 40, false},
		{"1Pi", 1 << 50, false},
		// 十进制后缀
		{"64M", 64000000, false},
		{"1G", 1000000000, false},
		{"64k", 64000, false},
		// 负值
		{"-1", 0, true},
		{"-64Mi", 0, true},
		// 非法格式
		{"abc", 0, true},
		{"64MiB", 0, true},
		{"0.5Gi", 0, true},
		{"1e6", 0, true},
		// 溢出
		{"9223372036854775807Ei", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseMemory(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseMemory(%q) 期望报错，实际 nil（got=%d）", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMemory(%q) 意外报错: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseMemory(%q) = %d, 期望 %d", tt.in, got, tt.want)
		}
	}
}

func TestParseMemory_KiBeforeK(t *testing.T) {
	// "64Ki" 必须以二进制单位解析（后缀从长到短匹配），不能先被 "K" 吃掉
	got, err := ParseMemory("64Ki")
	if err != nil || got != 64<<10 {
		t.Errorf("ParseMemory(\"64Ki\") = (%d, %v), 期望 (%d, nil)", got, err, 64<<10)
	}
}

func TestFormatCPU(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{250, "0.25"},
		{500, "0.5"},
		{1000, "1"},
		{1500, "1.5"},
		{0, "0"},
		{2000, "2"},
	}
	for _, tt := range tests {
		if got := FormatCPU(tt.in); got != tt.want {
			t.Errorf("FormatCPU(%d) = %q, 期望 %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatMemoryBytes(t *testing.T) {
	if got := FormatMemoryBytes(67108864); got != "67108864" {
		t.Errorf("FormatMemoryBytes(67108864) = %q, 期望 \"67108864\"", got)
	}
	if got := FormatMemorySwapBytes(67108864); got != "67108864" {
		t.Errorf("FormatMemorySwapBytes(67108864) = %q, 期望 \"67108864\"", got)
	}
}

func TestErrResourceExhaustedMarker(t *testing.T) {
	if ErrResourceExhausted == "" {
		t.Error("ErrResourceExhausted 标记不能为空")
	}
	// 云端靠 strings.Contains 识别，标记里不能出现会被 JSON 转义的字符
	if strings.ContainsAny(ErrResourceExhausted, `"\`) {
		t.Errorf("ErrResourceExhausted = %q 包含 JSON 转义敏感字符", ErrResourceExhausted)
	}
}
