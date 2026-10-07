package modelrepo

import "testing"

// v0.45.0（spec 0018 US-1）modality 白名单校验测试。

func TestValidateModelMetadataExt(t *testing.T) {
	cases := []struct {
		name    string
		m       map[string]string
		wantErr bool
	}{
		{"无键（存量兼容）", map[string]string{"k": "v"}, false},
		{"空值（视为缺省）", map[string]string{"modality": ""}, false},
		{"vision", map[string]string{"modality": "vision"}, false},
		{"time-series", map[string]string{"modality": "time-series"}, false},
		{"multimodal", map[string]string{"modality": "multimodal"}, false},
		{"非法值", map[string]string{"modality": "llm"}, true},
		{"大小写敏感", map[string]string{"modality": "Vision"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateModelMetadataExt(tc.m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateModelMetadataExt(%v) err=%v wantErr=%v", tc.m, err, tc.wantErr)
			}
		})
	}
	if _, ok := modalityWhitelist["vision"]; !ok {
		t.Fatal("vision 必须在白名单")
	}
}
