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
	// AddOrReplace 使用默认 inboundTag（GRPCXrayAPI 构造时传入的）。
	AddOrReplace(ctx context.Context, user *User) error
	// AddOrReplaceToTag 显式指定 inboundTag，用于多 inbound 场景。
	AddOrReplaceToTag(ctx context.Context, inboundTag string, user *User) error
	// RemoveUserById 使用默认 inboundTag。
	RemoveUserById(ctx context.Context, id string) error
	// RemoveUserFromTag 显式指定 inboundTag。
	RemoveUserFromTag(ctx context.Context, inboundTag, id string) error
	Close() error
}

type GRPCXrayAPI struct {
	addr       string
	inboundTag string
	conn       *grpc.ClientConn
	client     cmd.HandlerServiceClient
	timeout    time.Duration
}
