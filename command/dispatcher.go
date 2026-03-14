package command

import (
	"encoding/json"
	"log"

	"github.com/nats-io/nats.go"
)

// Handler 指令处理接口，新增指令只需实现此接口并注册
type Handler interface {
	Name() string
	Handle(data []byte) ([]byte, error)
}

// cmdMsg 服务端下发的指令格式：{"cmd":"xxx","data":{...}}
type cmdMsg struct {
	Cmd  string          `json:"cmd"`
	Data json.RawMessage `json:"data"`
}

// Dispatcher 路由 NATS 指令到对应 Handler
type Dispatcher struct {
	handlers map[string]Handler
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{handlers: make(map[string]Handler)}
}

func (d *Dispatcher) Register(h Handler) {
	d.handlers[h.Name()] = h
}

// Dispatch 作为 nats.MsgHandler 使用
func (d *Dispatcher) Dispatch(msg *nats.Msg) {
	var m cmdMsg
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		log.Printf("dispatcher: invalid cmd msg: %v", err)
		return
	}
	h, ok := d.handlers[m.Cmd]
	if !ok {
		log.Printf("dispatcher: unknown cmd=%s", m.Cmd)
		return
	}
	resp, err := h.Handle(m.Data)
	if err != nil {
		log.Printf("dispatcher: cmd=%s err=%v", m.Cmd, err)
	}
	// 若请求方需要回复（Request/Reply 模式）
	if msg.Reply != "" && resp != nil {
		if err := msg.Respond(resp); err != nil {
			log.Printf("dispatcher: respond cmd=%s err=%v", m.Cmd, err)
		}
	}
}
