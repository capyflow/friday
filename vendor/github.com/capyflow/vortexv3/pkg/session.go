package vpkg

// 用户的会话信息
type VSession struct {
	UID string `json:"uid"` // 用户ID
	SID string `json:"sid"` // 会话ID
	Exp int64  `json:"exp"` // 过期时间
	IP  string `json:"ip"`  // IP地址
}
