package vhttp

type Code struct {
	Code    int     `json:"code"`
	MsgKey  string  `json:"msg_key"`
	SubCode SubCode `json:"sub_code"`
}

// WithSubCode 设置子错误
func (c Code) WithSubCode(subCode SubCode) Code {
	c.SubCode = subCode
	return c
}

type SubCode struct {
	Code   int    `json:"code"`
	MsgKey string `json:"msg_key"`
}

var Codes = struct {
	Success        Code // 成功
	BadRequest     Code // 错误请求
	InternalError  Code // 内部错误
	PermissionDeny Code // 权限拒绝
	Unauthorized   Code // 未授权
}{
	Success:        Code{Code: 200, MsgKey: "success"},
	BadRequest:     Code{Code: 400, MsgKey: "bad_request"},
	InternalError:  Code{Code: 500, MsgKey: "internal_error"},
	PermissionDeny: Code{Code: 403, MsgKey: "permission_deny"},
	Unauthorized:   Code{Code: 401, MsgKey: "unauthorized"},
}
