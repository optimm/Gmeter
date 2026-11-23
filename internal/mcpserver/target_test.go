package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These check the policy at the point where a tool's input becomes a run
// config, with no server and no load involved.

func TestTargetRefusesAStreamAboveTheMessageCap(t *testing.T) {
	s := &service{policy: Policy{MaxStreamMessages: 10}}
	_, err := s.target(context.Background(), TargetInput{
		URL:  "127.0.0.1:50051",
		GRPC: &GRPCInput{Method: "gmeter.demo.Echo/Chat", SendMessages: 11},
	})
	if err == nil || !strings.Contains(err.Error(), "-max-stream-messages") {
		t.Fatalf("err = %v, want a refusal naming -max-stream-messages", err)
	}
	cfg, err := s.target(context.Background(), TargetInput{
		URL:  "127.0.0.1:50051",
		GRPC: &GRPCInput{Method: "gmeter.demo.Echo/Chat", SendMessages: 10, RecvMessages: 10},
	})
	if err != nil || cfg.GRPC == nil || cfg.GRPC.SendMessages != 10 {
		t.Fatalf("cfg.GRPC = %+v, err = %v; want the capped stream allowed", cfg.GRPC, err)
	}
}

func TestTargetRefusesATimeoutAboveTheLimit(t *testing.T) {
	s := &service{policy: Policy{MaxTimeout: 5 * time.Second}}
	_, err := s.target(context.Background(), TargetInput{URL: "http://127.0.0.1:8080", Timeout: "1m"})
	if err == nil || !strings.Contains(err.Error(), "-max-timeout") {
		t.Fatalf("err = %v, want a refusal naming -max-timeout", err)
	}
	cfg, err := s.target(context.Background(), TargetInput{URL: "http://127.0.0.1:8080", Timeout: "5s"})
	if err != nil || cfg.Timeout != 5*time.Second {
		t.Fatalf("cfg.Timeout = %v, err = %v; want 5s", cfg.Timeout, err)
	}
}
