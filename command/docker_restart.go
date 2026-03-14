package command

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
)

type DockerRestartHandler struct{}

type dockerRestartReq struct {
	Image     string `json:"image"`
	Container string `json:"container"`
}

type dockerRestartResp struct {
	Ok  bool   `json:"ok"`
	Msg string `json:"msg"`
}

func (DockerRestartHandler) Name() string { return "docker_restart" }

func (DockerRestartHandler) Handle(data []byte) ([]byte, error) {
	var req dockerRestartReq
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}
	if req.Image == "" || req.Container == "" {
		return errResp("image and container are required"), nil
	}

	if out, err := exec.Command("docker", "pull", req.Image).CombinedOutput(); err != nil {
		log.Printf("docker_restart: pull image=%s err=%v output=%s", req.Image, err, out)
		return errResp(fmt.Sprintf("docker pull failed: %v, output: %s", err, out)), nil
	}

	if out, err := exec.Command("docker", "restart", req.Container).CombinedOutput(); err != nil {
		log.Printf("docker_restart: restart container=%s err=%v output=%s", req.Container, err, out)
		return errResp(fmt.Sprintf("docker restart failed: %v, output: %s", err, out)), nil
	}
	log.Printf("docker_restart: container=%s OK", req.Container)

	return okResp("ok"), nil
}

func okResp(msg string) []byte {
	b, _ := json.Marshal(dockerRestartResp{Ok: true, Msg: msg})
	return b
}

func errResp(msg string) []byte {
	b, _ := json.Marshal(dockerRestartResp{Ok: false, Msg: msg})
	return b
}
