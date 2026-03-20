package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/salt-lake/kd-vps-agent/collect"
)

func newNATSConn(url, token string) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name("kd-node-agent"),
		nats.ReconnectWait(5 * time.Second),
		nats.MaxReconnects(-1),
	}
	if token != "" {
		opts = append(opts, nats.Token(token))
	}
	var nc *nats.Conn
	var err error
	for i := 0; i < 10; i++ {
		nc, err = nats.Connect(url, opts...)
		if err == nil {
			return nc, nil
		}
		log.Printf("connect nats failed (attempt %d/10): %v, retrying in 10s", i+1, err)
		time.Sleep(10 * time.Second)
	}
	return nil, fmt.Errorf("last error: %w", err)
}

func publish(nc *nats.Conn, subject string, p collect.Payload) {
	b, err := json.Marshal(p)
	if err != nil {
		log.Printf("publish: marshal failed: %v", err)
		return
	}
	if err := nc.Publish(subject, b); err != nil {
		log.Printf("publish: failed subject=%s err=%v", subject, err)
	}
}
