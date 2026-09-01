//go:build !sherpa

package voice

import (
	"fmt"

	"github.com/capyflow/friday/internal/config"
)

// newComponentsImpl 在未启用 sherpa 编译标签时的占位实现：
// 语音链路需要 cgo + 本地模型，用 -tags sherpa 编译才会启用。
func newComponentsImpl(_ *config.Config) (*Components, error) {
	return nil, fmt.Errorf("语音引擎需要 sherpa-onnx：请安装依赖并以 -tags sherpa 编译（见 README）")
}