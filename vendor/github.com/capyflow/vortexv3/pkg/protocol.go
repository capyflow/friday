package vpkg

import "encoding/json"

const (
	HTTP      = "http" // HTTP协议
	WEBSOCKET = "ws"   // WebSocket协议
	GRPC      = "grpc" // gRPC协议
)

// 相应和请求的结构体
type VStruct struct {
	Body any    `json:"body"` // 业务响应体
	Head VHead  `json:"head"` // 头信息
	Path string `json:"path"` // 请求路径
}

func (v *VStruct) Marshal() ([]byte, error) {
	return json.Marshal(v)
}

// VHead 头信息
// type VHead struct {
// 	Ec      int    `json:"ec"`       // 业务响应码
// 	Em      string `json:"em"`       // 业务响应消息
// 	TraceId string `json:"trace_id"` // 跟踪ID
// 	Ts      int64  `json:"ts"`       // 时间戳
// 	UID     string `json:"uid"`      // 目标用户ID（WS 推送时使用）
// 	SID     string `json:"sid"`      // 目标设备ID（WS 推送时使用，为空则推送到该用户所有设备）
// }

type VHead map[string]any

func (v VHead) SetEc(ec int) {
	v["ec"] = ec
}

func (v VHead) GetEc() int {
	switch val := v["ec"].(type) {
	case int:
		return val
	case int32:
		return int(val)
	case int64:
		return int(val)
	case float64:
		return int(val)
	case float32:
		return int(val)
	default:
		return 0
	}
}

func (v VHead) SetEm(em string) {
	v["em"] = em
}

func (v VHead) GetEm() string {
	em, ok := v["em"].(string)
	if !ok {
		return ""
	}
	return em
}

func (v VHead) SetTraceId(tid string) {
	v["trace_id"] = tid
}

func (v VHead) GetTraceId() string {
	tid, ok := v["trace_id"].(string)
	if !ok {
		return ""
	}
	return tid
}

func (v VHead) SetTs(ts int64) {
	v["ts"] = ts
}

func (v VHead) GetTs() int64 {
	switch val := v["ts"].(type) {
	case int64:
		return val
	case int:
		return int64(val)
	case int32:
		return int64(val)
	case float64:
		return int64(val)
	case float32:
		return int64(val)
	default:
		return 0
	}
}

func (v VHead) SetUID(uid string) {
	v["uid"] = uid
}

func (v VHead) GetUID() string {
	uid, ok := v["uid"].(string)
	if !ok {
		return ""
	}
	return uid
}

func (v VHead) SetSID(sid string) {
	v["sid"] = sid
}

func (v VHead) GetSID() string {
	sid, ok := v["sid"].(string)
	if !ok {
		return ""
	}
	return sid
}

func (v VHead) Get(key string) any {
	return v[key]
}

func (v VHead) Set(key string, value any) {
	v[key] = value
}

// VMsgResponse 消息响应
type VMsgResponse map[string]any

// I18nMap i18n映射
var i18nMap map[string]string

func SetI18nMap(i18n map[string]string) {
	i18nMap = i18n
}

func GetI18n(key string) string {
	return i18nMap[key]
}
