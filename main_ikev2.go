//go:build !xray

package main

import (
	"context"

	"github.com/salt-lake/kd-vps-agent/command"
)

func setupXray(_ context.Context, _ Config, _ *command.Dispatcher) func() {
	return nil
}
