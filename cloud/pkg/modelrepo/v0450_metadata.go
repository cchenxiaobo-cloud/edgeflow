package modelrepo

// v0.45.0（spec 0018 US-1/US-2，G23）：模型面扩展元数据约定。
//
// 设计：Modality 与场景绑定、时序输入约定、训练闭环留位全部以 Metadata
// 约定键承载（契约"只增不改"——存量模型零迁移；发布平铺进 config-sync 的
// 既有行为自动携带新键）。
//
// 约定键（键名符合既有 metadataKeyRe ^[A-Za-z0-9._-]{1,64}$）：
//   - modality           模态：vision（缺省）| time-series | multimodal
//   - scene.bindings     场景绑定：JSON 数组 [{"kind":"camera|device","name":"..."}]
//   - input.features     时序输入特征名（逗号分隔）
//   - input.window       时序窗口长度（样本数，十进制）
//   - input.rate         时序采样率（Hz，十进制）
//   - train.dataset-ref  训练闭环留位：数据集引用（仅约定，不做训练服务）
//   - train.job-id       训练闭环留位：训练任务 ID

// Modality 常量（modality 白名单）。
const (
	ModalityVision     = "vision"
	ModalityTimeSeries = "time-series"
	ModalityMultimodal = "multimodal"
)

// metadata modality 约定键。
const MetaKeyModality = "modality"

// modalityWhitelist 是合法模态集合（空值视为缺省 vision——兼容存量）。
var modalityWhitelist = map[string]bool{
	ModalityVision:     true,
	ModalityTimeSeries: true,
	ModalityMultimodal: true,
}

// ValidateModelMetadataExt 是 v0.45.0 扩展校验：在既有 ValidateMetadata
// （键/值格式）之上追加 modality 白名单语义。存量模型不受影响（无该键即跳过）；
// CreateModel/UpdateModel 装配层调用（放在 ValidateMetadata 之后）。
func ValidateModelMetadataExt(m map[string]string) error {
	mod, ok := m[MetaKeyModality]
	if !ok || mod == "" {
		return nil // 缺省 vision（兼容存量；键可省略）
	}
	if !modalityWhitelist[mod] {
		return &InvalidModalityError{Value: mod}
	}
	return nil
}

// InvalidModalityError 是 modality 白名单校验失败（400 语义，装配层映射）。
type InvalidModalityError struct {
	Value string
}

func (e *InvalidModalityError) Error() string {
	return "invalid modality " + quote(e.Value) + ": must be one of vision|time-series|multimodal"
}

func quote(s string) string {
	b := []byte{'"'}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			b = append(b, '\\', c)
			continue
		}
		if c < 0x20 {
			b = append(b, []byte("\\x")...)
			const hexd = "0123456789abcdef"
			b = append(b, hexd[c>>4], hexd[c&0xf])
			continue
		}
		b = append(b, c)
	}
	return string(append(b, '"'))
}
