//go:build xray

package xray

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// HTTPAPI 暴露给业务服务器的 xray 用户增删 HTTP 接口。
//
//	POST   /xray/users          body {"uuid":"..."}   add
//	DELETE /xray/users/{uuid}                          remove
//
// 鉴权：Authorization: Bearer <SCRIPT_TOKEN>
type HTTPAPI struct {
	syncer userOps
	token  string
}

type userOps interface {
	AddUser(uuid string) error
	RemoveUser(uuid string) error
}

func NewHTTPAPI(s userOps, token string) *HTTPAPI {
	return &HTTPAPI{syncer: s, token: token}
}

func (h *HTTPAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	const prefix = "/xray/users"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)

	switch {
	case r.Method == http.MethodPost && (rest == "" || rest == "/"):
		h.handleAdd(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(rest, "/"):
		h.handleRemove(w, strings.TrimPrefix(rest, "/"))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// checkAuth 用恒定时间比较防止 token 被定时攻击枚举。
func (h *HTTPAPI) checkAuth(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

func (h *HTTPAPI) handleAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID string `json:"uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if body.UUID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uuid required"})
		return
	}
	if err := h.syncer.AddUser(body.UUID); err != nil {
		log.Printf("http_api: add uuid=%s err=%v", body.UUID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPAPI) handleRemove(w http.ResponseWriter, uuid string) {
	if uuid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uuid required"})
		return
	}
	if err := h.syncer.RemoveUser(uuid); err != nil {
		log.Printf("http_api: remove uuid=%s err=%v", uuid, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// StartHTTPAPI 在 addr 启动 HTTP server，ctx 取消时优雅关闭。
// addr 为空 / token 为空时不启动，仅记录一行日志。
func StartHTTPAPI(ctx context.Context, addr, token string, syncer userOps) {
	if addr == "" {
		log.Println("xray http api disabled: HTTP_API_ADDR not set")
		return
	}
	if token == "" {
		log.Println("xray http api disabled: SCRIPT_TOKEN not set")
		return
	}
	srv := &http.Server{
		Addr:         addr,
		Handler:      NewHTTPAPI(syncer, token),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	go func() {
		log.Printf("xray http api listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("xray http api: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sdCtx)
	}()
}
