package command

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"time"
)

type swanUpdateReq struct {
	Image string `json:"image"`
}

// SwanUpdateHandler 处理 swan_update 指令：拉取新镜像（可选）并重启 StrongSwan 容器。
type SwanUpdateHandler struct {
	container    string
	defaultImage string
}

func NewSwanUpdateHandler(container, defaultImage string) SwanUpdateHandler {
	return SwanUpdateHandler{container: container, defaultImage: defaultImage}
}

func (SwanUpdateHandler) Name() string { return "swan_update" }

func (h SwanUpdateHandler) Handle(data []byte) ([]byte, error) {
	var req swanUpdateReq
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}

	image := req.Image
	if image == "" {
		image = h.defaultImage
	}
	if image != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); err != nil {
			log.Printf("swan_update: pull image=%s err=%v output=%s", image, err, out)
			return errResp(fmt.Sprintf("docker pull failed: %v, output: %s", err, out)), nil
		}
	}

	if out, err := exec.Command("docker", "restart", h.container).CombinedOutput(); err != nil {
		log.Printf("swan_update: restart container=%s err=%v output=%s", h.container, err, out)
		return errResp(fmt.Sprintf("docker restart failed: %v, output: %s", err, out)), nil
	}

	log.Printf("swan_update: container=%s OK", h.container)
	return okResp("ok"), nil
}

type cmdResp struct {
	Ok  bool   `json:"ok"`
	Msg string `json:"msg"`
}

func okResp(msg string) []byte {
	b, _ := json.Marshal(cmdResp{Ok: true, Msg: msg})
	return b
}

func errResp(msg string) []byte {
	b, _ := json.Marshal(cmdResp{Ok: false, Msg: msg})
	return b
}
