package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/agent"
	"github.com/optimm/Gmeter/internal/pacer"
)

const agentPassword = "a-password-for-the-agent-tests"

// lockedBuffer is a bytes.Buffer that a running command and a test can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startAgent runs `gmeter agent` until the test ends and returns its address.
func startAgent(t *testing.T, args ...string) string {
	t.Helper()
	t.Setenv(tokenEnv, agentPassword)
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, append([]string{"agent"}, args...), &bytes.Buffer{}, &stderr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("the agent exited %d when stopped:\n%s", code, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Error("the agent did not stop")
		}
	})
	listening := regexp.MustCompile(`listening on (\S+)`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := listening.FindStringSubmatch(stderr.String()); m != nil {
			return m[1]
		}
		select {
		case code := <-done:
			t.Fatalf("the agent exited %d:\n%s", code, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("the agent never said where it listens:\n%s", stderr.String())
	return ""
}

func TestTheAgentCommandServesJobsUntilStopped(t *testing.T) {
	addr := startAgent(t, "-listen", "127.0.0.1:0")
	st, err := agent.NewClient(addr, agentPassword).Ping(context.Background())
	if err != nil || st.Version != agent.Version || st.Busy {
		t.Errorf("Ping = %+v, %v; want an idle agent", st, err)
	}
	if _, err := agent.NewClient(addr, agentPassword+"x").Ping(context.Background()); err == nil {
		t.Error("the agent answered a caller with the wrong password")
	}
}

func TestTheAgentCommandListensOnlyOnThisMachineByDefault(t *testing.T) {
	if defaultAgentListen != "127.0.0.1:7070" {
		t.Errorf("the default is %s: an agent should not be reachable from the network unless asked", defaultAgentListen)
	}
}

func TestTheAgentCommandWarnsWhenReachableFromTheNetwork(t *testing.T) {
	t.Setenv(tokenEnv, agentPassword)
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"agent", "-listen", ":0"}, &bytes.Buffer{}, &stderr) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "listening on") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(stderr.String(), "not encrypted") {
		t.Errorf("no warning that the password and traffic go in the clear:\n%s", stderr.String())
	}
}

func TestTheAgentCommandWantsAPassword(t *testing.T) {
	for _, pw := range []string{"", "short"} {
		t.Setenv(tokenEnv, pw)
		code, _, stderr := exec(t, "agent", "-listen", "127.0.0.1:0")
		if code != 2 || !strings.Contains(stderr, tokenEnv) {
			t.Errorf("password %q: exit %d, stderr %q; want 2 and the variable to set", pw, code, stderr)
		}
	}
}

func TestTheAgentCommandSaysWhenItCannotListen(t *testing.T) {
	addr := startAgent(t, "-listen", "127.0.0.1:0")
	code, _, stderr := exec(t, "agent", "-listen", addr) // taken
	if code != 1 || !strings.Contains(stderr, addr) {
		t.Errorf("exit %d, stderr %q; want 1 and the address", code, stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if code := run(ctx, []string{"agent", "-listen", "127.0.0.1:0", "extra"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Errorf("an argument: exit %d, want 2", code)
	}
}

func TestStoppingTheAgentCommandStopsItsJob(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()

	t.Setenv(tokenEnv, agentPassword)
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"agent", "-listen", "127.0.0.1:0"}, &bytes.Buffer{}, &stderr) }()
	var addr string
	for deadline := time.Now().Add(5 * time.Second); addr == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if m := regexp.MustCompile(`listening on (\S+)`).FindStringSubmatch(stderr.String()); m != nil {
			addr = m[1]
		}
	}
	ran := make(chan error, 1)
	go func() {
		_, err := agent.NewClient(addr, agentPassword).Run(context.Background(), agent.Job{
			URL: target.URL, Stages: pacer.ConstantRate(200, time.Minute), Workers: 4, Timeout: time.Second, SampleEvery: time.Second,
		})
		ran <- err
	}()
	for hits.Load() < 20 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-ran:
		if err == nil {
			t.Error("the controller got a result from a job that was stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the controller was not told the job stopped")
	}
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	after := hits.Load()
	time.Sleep(300 * time.Millisecond)
	if more := hits.Load() - after; more > 0 {
		t.Errorf("%d requests were sent after the agent stopped", more)
	}
}
