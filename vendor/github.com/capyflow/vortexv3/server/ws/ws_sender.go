package vws

import vpkg "github.com/capyflow/vortexv3/pkg"

// MessageSender WS 消息发送接口，由 VortexEngine 自动实现。
// 业务代码依赖此接口即可发送 WS 消息，无需直接引用 engine。
type MessageSender interface {
	SendToUser(uid string, data *vpkg.VStruct) error
	SendToDevice(uid, sid string, data *vpkg.VStruct) error
	Broadcast(data *vpkg.VStruct) error
}
