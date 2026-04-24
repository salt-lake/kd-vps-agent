//go:build xray

package command

import (
	"encoding/json"
	"log"
)

// XrayUserManager 定义 command 包对 xray 用户管理的依赖接口，
// 避免直接 import sync 包（依赖倒置）。
type XrayUserManager interface {
	AddUser(uuid, tier string) error
	RemoveUser(uuid string) error
}

type xrayUserPayload struct {
	UUID  string `json:"uuid"`
	Email string `json:"email"`
	Tier  string `json:"tier,omitempty"` // 老后端发送空字符串，走兼容路径
}

// XrayUserAddHandler 处理 xray_user_add 指令。
type XrayUserAddHandler struct {
	syncer XrayUserManager
}

func NewXrayUserAddHandler(s XrayUserManager) XrayUserAddHandler {
	return XrayUserAddHandler{syncer: s}
}

func (XrayUserAddHandler) Name() string { return "xray_user_add" }

func (h XrayUserAddHandler) Handle(data []byte) ([]byte, error) {
	if h.syncer == nil {
		return errResp("xray syncer not available"), nil
	}
	var p xrayUserPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}
	if p.UUID == "" {
		return errResp("uuid is required"), nil
	}
	if err := h.syncer.AddUser(p.UUID, p.Tier); err != nil {
		log.Printf("xray_user_add: uuid=%s tier=%s err=%v", p.UUID, p.Tier, err)
		return errResp(err.Error()), nil
	}
	return okResp("ok"), nil
}

// XrayUserRemoveHandler 处理 xray_user_remove 指令。
type XrayUserRemoveHandler struct {
	syncer XrayUserManager
}

func NewXrayUserRemoveHandler(s XrayUserManager) XrayUserRemoveHandler {
	return XrayUserRemoveHandler{syncer: s}
}

func (XrayUserRemoveHandler) Name() string { return "xray_user_remove" }

func (h XrayUserRemoveHandler) Handle(data []byte) ([]byte, error) {
	if h.syncer == nil {
		return errResp("xray syncer not available"), nil
	}
	var p xrayUserPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}
	if p.UUID == "" {
		return errResp("uuid is required"), nil
	}
	if err := h.syncer.RemoveUser(p.UUID); err != nil {
		log.Printf("xray_user_remove: uuid=%s err=%v", p.UUID, err)
		return errResp(err.Error()), nil
	}
	return okResp("ok"), nil
}
