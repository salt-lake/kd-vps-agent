package command

import (
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
)

// fakeHandler 记录被调用的情况
type fakeHandler struct {
	name    string
	called  bool
	retResp []byte
	retErr  error
}

func (f *fakeHandler) Name() string { return f.name }
func (f *fakeHandler) Handle(_ []byte) ([]byte, error) {
	f.called = true
	return f.retResp, f.retErr
}

func makeMsg(cmd string, data string) *nats.Msg {
	payload, _ := json.Marshal(map[string]interface{}{
		"cmd":  cmd,
		"data": json.RawMessage(data),
	})
	return &nats.Msg{Data: payload}
}

func TestDispatcher_KnownCmd(t *testing.T) {
	d := NewDispatcher()
	h := &fakeHandler{name: "test_cmd", retResp: okResp("done")}
	d.Register(h)

	d.Dispatch(makeMsg("test_cmd", `{}`))

	if !h.called {
		t.Error("expected handler to be called")
	}
}

func TestDispatcher_UnknownCmd(t *testing.T) {
	d := NewDispatcher()
	// 不注册任何 handler，发送未知指令不应 panic
	d.Dispatch(makeMsg("no_such_cmd", `{}`))
}

func TestDispatcher_InvalidJSON(t *testing.T) {
	d := NewDispatcher()
	// 非法 JSON 不应 panic
	d.Dispatch(&nats.Msg{Data: []byte("not json")})
}

func TestDispatcher_Register_Overwrite(t *testing.T) {
	d := NewDispatcher()
	h1 := &fakeHandler{name: "dup", retResp: []byte(`{"ok":true,"msg":"h1"}`)}
	h2 := &fakeHandler{name: "dup", retResp: []byte(`{"ok":true,"msg":"h2"}`)}
	d.Register(h1)
	d.Register(h2) // 覆盖 h1

	d.Dispatch(makeMsg("dup", `{}`))

	if h1.called {
		t.Error("h1 should not be called after being overwritten")
	}
	if !h2.called {
		t.Error("h2 should be called as the latest registration")
	}
}

func TestDispatcher_ReplyMode(t *testing.T) {
	d := NewDispatcher()
	h := &fakeHandler{name: "reply_cmd", retResp: okResp("pong")}
	d.Register(h)

	// Reply 为空时不应 panic（无需实际 NATS 连接）
	msg := makeMsg("reply_cmd", `{}`)
	msg.Reply = "" // 无回复主题
	d.Dispatch(msg)

	if !h.called {
		t.Error("expected handler to be called")
	}
}
