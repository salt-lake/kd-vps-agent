package xray

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cmd "github.com/salt-lake/kd-vps-agent/xrayproto/app/proxyman/command"
	stats "github.com/salt-lake/kd-vps-agent/xrayproto/app/stats/command"
	"github.com/salt-lake/kd-vps-agent/xrayproto/common/protocol"
	"github.com/salt-lake/kd-vps-agent/xrayproto/common/serial"
	"github.com/salt-lake/kd-vps-agent/xrayproto/proxy/vless"
)

const DefaultXrayRPCTimeout = 1 * time.Second

func NewGRPCXrayAPI(addr, inboundTag string) (*GRPCXrayAPI, error) {
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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

	a.conn.Connect()
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
	rpcCtx, rpcCancel := context.WithTimeout(cctx, DefaultXrayRPCTimeout)
	defer rpcCancel()

	client := stats.NewStatsServiceClient(a.conn)
	_, err := client.GetStats(rpcCtx, &stats.GetStatsRequest{
		Name:   "inbound>>>__health_check__>>>traffic>>>uplink",
		Reset_: false,
	})
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
	if err := a.insertUser(ctx, user); err == nil {
		return nil
	} else if !isXrayAlreadyExists(err) {
		return err
	}
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
		Operation: toTypedMessage(op),
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	_, err := a.client.AlterInbound(cctx, req)
	if err != nil && !isXrayNotFound(err) {
		log.Printf("[XRAY] remove_user failed addr=%s inbound=%s email=%s err=%v",
			a.addr, a.inboundTag, email, err)
		return err
	}
	return nil
}

func (a *GRPCXrayAPI) insertUser(ctx context.Context, user *User) error {
	prUser, err := buildProtocolUser(user)
	if err != nil {
		return err
	}
	op := &cmd.AddUserOperation{User: prUser}
	req := &cmd.AlterInboundRequest{
		Tag:       a.inboundTag,
		Operation: toTypedMessage(op),
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

func buildProtocolUser(u *User) (*protocol.User, error) {
	if _, err := uuid.Parse(u.UUID); err != nil {
		return nil, fmt.Errorf("invalid uuid %q: %w", u.UUID, err)
	}
	acc := &vless.Account{
		Id:         u.UUID,
		Encryption: "none",
		Flow:       u.Flow,
	}
	return &protocol.User{
		Level:   0,
		Email:   xrayEmail(u.ID),
		Account: toTypedMessage(acc),
	}, nil
}

// toTypedMessage 将 proto.Message 序列化为 TypedMessage（等价于 xray-core/common/serial.ToTypedMessage）
func toTypedMessage(m proto.Message) *serial.TypedMessage {
	b, _ := proto.Marshal(m)
	return &serial.TypedMessage{
		Type:  string(m.ProtoReflect().Descriptor().FullName()),
		Value: b,
	}
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
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "duplicate")
}

func isXrayNotFound(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	if st.Code() == codes.NotFound {
		return true
	}
	return strings.Contains(strings.ToLower(st.Message()), "not found")
}

func xrayEmail(id string) string {
	return fmt.Sprintf("xray@%s", id)
}
