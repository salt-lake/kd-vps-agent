package xray

import (
	"time"

	cmd "github.com/xtls/xray-core/app/proxyman/command"
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
}

type GRPCXrayAPI struct {
	addr       string // 127.0.0.1:10085
	inboundTag string // 你要管理哪个 inbound 的 tag（例如 "vless-in"）
	conn       *grpc.ClientConn
	client     cmd.HandlerServiceClient
	timeout    time.Duration
}
