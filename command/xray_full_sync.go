//go:build xray

package command

import "log"

// XrayFullSyncer 依赖接口，避免直接 import xray 包。
type XrayFullSyncer interface {
	FullSync() error
}

// XrayFullSyncHandler 处理 xray_full_sync 指令：触发一次全量用户同步。
type XrayFullSyncHandler struct {
	syncer XrayFullSyncer
}

func NewXrayFullSyncHandler(s XrayFullSyncer) XrayFullSyncHandler {
	return XrayFullSyncHandler{syncer: s}
}

func (XrayFullSyncHandler) Name() string { return "xray_full_sync" }

func (h XrayFullSyncHandler) Handle(_ []byte) ([]byte, error) {
	if h.syncer == nil {
		return errResp("xray syncer not available"), nil
	}
	log.Println("xray_full_sync: triggered by remote command")
	if err := h.syncer.FullSync(); err != nil {
		log.Printf("xray_full_sync: err=%v", err)
		return errResp(err.Error()), nil
	}
	return okResp("ok"), nil
}
