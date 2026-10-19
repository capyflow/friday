package vortex

import (
	vpkg "github.com/capyflow/vortexv3/pkg"
)

// SendToUser 发送消息给指定用户的所有设备
func (v *VortexEngine) SendToUser(uid string, data *vpkg.VStruct) error {
	return v.WsManager.PushToUser(uid, data)
}

// SendToDevice 发送消息给指定用户的特定设备
func (v *VortexEngine) SendToDevice(uid, sid string, data *vpkg.VStruct) error {
	return v.WsManager.PushToDevice(uid, sid, data)
}

// Broadcast 广播消息给所有在线连接
func (v *VortexEngine) Broadcast(data *vpkg.VStruct) error {
	return v.WsManager.Broadcast(data)
}
