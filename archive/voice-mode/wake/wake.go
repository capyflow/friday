// Package wake 定义唤醒词检测抽象：机器人平时待机监听，听到唤醒词才进入对话。
package wake

import (
	"context"
	"os"
	"strings"
)

// Detector 持续监听麦克风，检测到唤醒词后返回。
type Detector interface {
	// Wait 阻塞直到检测到唤醒词（返回 nil）或 ctx 取消。
	Wait(ctx context.Context) error
	Close() error
}

// KeywordsFromFile 解析模型目录下 keywords.txt 的生效唤醒词
// （每行"拼音标注 @中文词"，取 @ 之后的词）。
func KeywordsFromFile(modelDir string) ([]string, error) {
	data, err := os.ReadFile(modelDir + "/keywords.txt")
	if err != nil {
		return nil, err
	}
	var words []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.LastIndex(line, "@"); i >= 0 {
			if w := strings.TrimSpace(line[i+1:]); w != "" {
				words = append(words, w)
			}
		}
	}
	return words, nil
}
