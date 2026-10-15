package config

import (
	"errors"
	"fmt"
	"os"
)

// EnsureInitial 只播种真正不存在的配置，不覆盖损坏文件或悬空链接。
// 此配置无密钥，且 provider 的两个值是**明显示例占位**（不是某家真实服务商的默认值，
// 免得用户以为"已经给我配好了 DeepSeek"）——因为 Validate() 要求 baseURL/model 非空，
// 留空会让服务起不来，所以用一眼看得出要改的占位串，由用户在「设置 → 模型」里替换。
func EnsureInitial(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("创建初始配置失败: %w", err)
	}
	const initial = "provider:\n  name: openai\n  baseURL: https://api.example.com/v1\n  model: 请填模型名\n  apiKeyEnv: FRIDAY_API_KEY\nadmin:\n  host: 127.0.0.1\n  port: 8080\n"
	_, err = f.WriteString(initial)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
