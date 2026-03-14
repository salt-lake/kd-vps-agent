package xray

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	stats "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/proxy/vless"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	// Xray protobuf / types
	cmd "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	xuuid "github.com/xtls/xray-core/common/uuid"
)

const DefaultXrayRPCTimeout = 1 * time.Second

func NewGRPCXrayAPI(addr, inboundTag string) (*GRPCXrayAPI, error) {
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()), // 本地一般 plaintext
	)
	if err != nil {
		return nil, err
	}
	return &GRPCXrayAPI{
		addr:       addr,
		inboundTag: inboundTag,
		conn:       conn,
		client:     cmd.NewHandlerServiceClient(conn),
		timeout:    10 * time.Second,
	}, nil
}

func (a *GRPCXrayAPI) Close() error {
	if a.conn != nil {
		return a.conn.Close()
	}
	return nil
}

func (a *GRPCXrayAPI) IsXrayReady(ctx context.Context) bool {
	if a == nil || a.conn == nil {
		return false
	}
	// 兜底超时：上层没 deadline 就用 a.timeout / 默认 2s
	cctx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		to := a.timeout
		if to <= 0 {
			to = 2 * time.Second
		}
		cctx, cancel = context.WithTimeout(ctx, to)
	}
	defer cancel()

	// 主动 exit idle（开始拨号）
	a.conn.Connect()
	// 等连接 READY（或超时/取消）
	for {
		s := a.conn.GetState()
		if s == connectivity.Ready {
			break
		}
		if s == connectivity.Shutdown {
			return false
		}
		if !a.conn.WaitForStateChange(cctx, s) {
			return false
		}
	}
	// 真 RPC 健康检查（StatsService）
	rpcCtx, rpcCancel := context.WithTimeout(cctx, DefaultXrayRPCTimeout)
	defer rpcCancel()

	client := stats.NewStatsServiceClient(a.conn)
	_, err := client.GetStats(rpcCtx, &stats.GetStatsRequest{
		Name:   "inbound>>>__health_check__>>>traffic>>>uplink",
		Reset_: false,
	})
	// 成功 or NotFound 都算 ready
	return err == nil || isXrayNotFound(err)
}

func (a *GRPCXrayAPI) AddBatch(ctx context.Context, users []*User) error {
	for _, u := range users {
		if err := a.AddOrReplace(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

func (a *GRPCXrayAPI) AddOrReplace(ctx context.Context, user *User) error {
	// 1) 快路径：直接 Add
	if err := a.insertUser(ctx, user); err == nil {
		return nil
	} else {
		// 不是已存在 -> 直接失败
		if !isXrayAlreadyExists(err) {
			return err
		}
	}
	// 2) 慢路径：已存在 -> Remove 再 Add
	if err := a.RemoveUserById(ctx, user.ID); err != nil {
		return err
	}
	return a.insertUser(ctx, user)
}

func (a *GRPCXrayAPI) RemoveUserById(ctx context.Context, id string) error {
	email := xrayEmail(id)
	op := &cmd.RemoveUserOperation{Email: email}
	req := &cmd.AlterInboundRequest{
		Tag:       a.inboundTag,
		Operation: serial.ToTypedMessage(op),
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	_, err := a.client.AlterInbound(cctx, req)
	if err != nil {
		// 如果是用户不存在，也当作删除成功
		if isXrayNotFound(err) {
			return nil
		}
		log.Printf("[XRAY] remove_user failed addr=%s inbound=%s email=%s err=%v",
			a.addr, a.inboundTag, email, err)
	}
	return err
}

func (a *GRPCXrayAPI) insertUser(ctx context.Context, user *User) error {
	prUser, err := buildProtocolUser(user)
	if err != nil {
		return err
	}
	op := &cmd.AddUserOperation{User: prUser}
	req := &cmd.AlterInboundRequest{
		Tag:       a.inboundTag,
		Operation: serial.ToTypedMessage(op),
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	_, err = a.client.AlterInbound(cctx, req)
	if err != nil {
		log.Printf("[XRAY] add_user failed addr=%s inbound=%s email=%s id=%s uuid=%s err=%v",
			a.addr, a.inboundTag, prUser.Email, user.ID, user.UUID, err)
	}
	return err
}

// RemoveUserByEmail 注意：Xray 的 RemoveUserOperation 通常是按 “email” 删除用户（不是 UUID）。
// 你这里的 userID 建议直接传你当初设置进 User.Email 的值。

func buildProtocolUser(u *User) (*protocol.User, error) {
	// vless.MemoryAccount：ID / Flow / Encryption（常见：Encryption = "none"）
	if _, err := xuuid.ParseString(u.UUID); err != nil {
		return nil, fmt.Errorf("invalid uuid %q: %w", u.UUID, err)
	}
	email := xrayEmail(u.ID)
	acc := &vless.Account{
		Id:         u.UUID, // u.UUID 这里建议你存 uuid.UUID 类型；如果是 string，你自己 parse 一下
		Encryption: "none",
	}
	if u.Flow != "" {
		acc.Flow = u.Flow
	}
	return &protocol.User{
		Level:   0,
		Email:   email, // 删除用户靠它
		Account: serial.ToTypedMessage(acc),
	}, nil
}

func isXrayAlreadyExists(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}

	if st.Code() == codes.AlreadyExists {
		return true
	}

	msg := strings.ToLower(st.Message())
	// 兼容你贴的报错："... already exists."
	if strings.Contains(msg, "already exists") {
		return true
	}
	// 保险：有些实现会说 duplicate / exists
	if strings.Contains(msg, "duplicate") || strings.Contains(msg, " already exist") || strings.Contains(msg, " exists") {
		return true
	}
	return false
}

func isXrayNotFound(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	if st.Code() == codes.NotFound {
		return true
	}
	msg := strings.ToLower(st.Message())
	return strings.Contains(msg, "not found")
}

func xrayEmail(id string) string {
	return fmt.Sprintf("xray@%s", id)
}
