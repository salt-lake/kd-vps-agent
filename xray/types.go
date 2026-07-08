//go:build xray

package xray

import (
	"time"

	cmd "github.com/salt-lake/kd-vps-agent/xray/proto/app/proxyman/command"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

type User struct {
	ID        string    `json:"id"`
	UUID      string    `json:"uuid"`
	Flow      string    `json:"flow,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type XrayAPI interface {
	IsXrayReady(ctx context.Context) bool
	AddBatch(ctx context.Context, users []*User) error
	AddOrReplace(ctx context.Context, user *User) error
	RemoveUserById(ctx context.Context, id string) error
	Close() error
}

// InboundSpec 描述一个需下发用户的 inbound：tag + 协议（决定账号类型）。
type InboundSpec struct {
	Tag      string
	Protocol string // "vless" | "hysteria"
}

type GRPCXrayAPI struct {
	addr     string
	inbounds []InboundSpec
	conn     *grpc.ClientConn
	client   cmd.HandlerServiceClient
	timeout  time.Duration
}
