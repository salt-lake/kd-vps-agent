//go:build xray

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

	cmd "github.com/salt-lake/kd-vps-agent/xray/proto/app/proxyman/command"
	stats "github.com/salt-lake/kd-vps-agent/xray/proto/app/stats/command"
	"github.com/salt-lake/kd-vps-agent/xray/proto/common/protocol"
	"github.com/salt-lake/kd-vps-agent/xray/proto/common/serial"
	hyacct "github.com/salt-lake/kd-vps-agent/xray/proto/proxy/hysteria/account"
	"github.com/salt-lake/kd-vps-agent/xray/proto/proxy/vless"
)

const DefaultXrayRPCTimeout = 1 * time.Second

func NewGRPCXrayAPI(addr string, inbounds []InboundSpec) (*GRPCXrayAPI, error) {
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	return &GRPCXrayAPI{
		addr:     addr,
		inbounds: inbounds,
		conn:     conn,
		client:   cmd.NewHandlerServiceClient(conn),
		timeout:  10 * time.Second,
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
	if err := a.insertUser(ctx, user); err != nil && !isXrayAlreadyExists(err) {
		return err
	}
	return nil
}

func (a *GRPCXrayAPI) RemoveUserById(ctx context.Context, id string) error {
	email := xrayEmail(id)
	// 遍历所有 inbound（vless + hy2），每个各删一次；全部成功才算完成。
	for _, ib := range a.inbounds {
		op := &cmd.RemoveUserOperation{Email: email}
		req := &cmd.AlterInboundRequest{
			Tag:       ib.Tag,
			Operation: toTypedMessage(op),
		}
		cctx, cancel := context.WithTimeout(ctx, a.timeout)
		_, err := a.client.AlterInbound(cctx, req)
		cancel()
		if err != nil && !isXrayNotFound(err) {
			log.Printf("[XRAY] remove_user failed addr=%s inbound=%s email=%s err=%v",
				a.addr, ib.Tag, email, err)
			return err
		}
	}
	return nil
}

func (a *GRPCXrayAPI) insertUser(ctx context.Context, user *User) error {
	// 遍历所有 inbound（vless + hy2），按协议构造对应账号，各加一次；全部成功才算完成。
	for _, ib := range a.inbounds {
		prUser, err := buildProtocolUser(user, ib.Protocol)
		if err != nil {
			return err
		}
		op := &cmd.AddUserOperation{User: prUser}
		req := &cmd.AlterInboundRequest{
			Tag:       ib.Tag,
			Operation: toTypedMessage(op),
		}
		cctx, cancel := context.WithTimeout(ctx, a.timeout)
		_, err = a.client.AlterInbound(cctx, req)
		cancel()
		if err != nil && !isXrayAlreadyExists(err) {
			log.Printf("[XRAY] add_user failed addr=%s inbound=%s proto=%s email=%s id=%s uuid=%s err=%v",
				a.addr, ib.Tag, ib.Protocol, prUser.Email, user.ID, user.UUID, err)
			return err
		}
	}
	return nil
}

func buildProtocolUser(u *User, protocolName string) (*protocol.User, error) {
	if _, err := uuid.Parse(u.UUID); err != nil {
		return nil, fmt.Errorf("invalid uuid %q: %w", u.UUID, err)
	}
	var acc proto.Message
	switch protocolName {
	case "hysteria":
		acc = &hyacct.Account{Auth: u.UUID}
	default: // vless
		acc = &vless.Account{
			Id:         u.UUID,
			Encryption: "none",
			Flow:       u.Flow,
		}
	}
	return &protocol.User{
		Level:   0,
		Email:   xrayEmail(u.ID),
		Account: toTypedMessage(acc),
	}, nil
}

// toTypedMessage 将 proto.Message 序列化为 TypedMessage（等价于 xray-core/common/serial.ToTypedMessage）
func toTypedMessage(m proto.Message) *serial.TypedMessage {
	b, err := proto.Marshal(m)
	if err != nil {
		log.Printf("toTypedMessage: marshal %T failed: %v", m, err)
	}
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
