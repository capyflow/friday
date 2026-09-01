package reminder

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

func newID() string {
	return fmt.Sprintf("r%d", time.Now().UnixNano())
}

func sortByWhen(rs []Reminder) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].When.Before(rs[j].When) })
}

// ParseHumanDuration 把用户输入的人类可读时长（"10分钟后"）解析为 time.Duration。
func ParseHumanDuration(s string) (time.Duration, error) {
	orig := s
	s = stripSpace(s)
	repl := map[string]string{
		"天":       "d",
		"days":     "d",
		"小时":     "h",
		"hours":    "h",
		"时":       "h",
		"分钟":     "m",
		"minutes":  "m",
		"分":       "m",
		"秒钟":     "s",
		"seconds":  "s",
		"秒":       "s",
	}
	for _, k := range unitKeys(repl) {
		s = replaceAll(s, k, repl[k])
	}
	if d, err := parseTokens(s); err == nil {
		return d, nil
	}
	return 0, fmt.Errorf("无法解析时长 %q", orig)
}

// parseTokens 手工解析"数字+单位"序列（如 1d2h30m、10m、90s）。
// 不依赖 time.ParseDuration，因为其不支持"天"（d）单位。
func parseTokens(s string) (time.Duration, error) {
	var total time.Duration
	i := 0
	for i < len(s) {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("缺少数字")
		}
		n, _ := strconv.Atoi(s[i:j])
		k := j
		for k < len(s) && (s[k] < '0' || s[k] > '9') {
			k++
		}
		var mult time.Duration
		switch s[j:k] {
		case "d":
			mult = 24 * time.Hour
		case "h":
			mult = time.Hour
		case "m":
			mult = time.Minute
		case "ms":
			mult = time.Millisecond
		case "s":
			mult = time.Second
		default:
			return 0, fmt.Errorf("未知单位 %q", s[j:k])
		}
		total += time.Duration(n) * mult
		i = k
	}
	return total, nil
}

// unitKeys 返回按长度降序的 key 列表，保证"分钟"先于"分"替换。
func unitKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return keys
}

func stripSpace(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func replaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
