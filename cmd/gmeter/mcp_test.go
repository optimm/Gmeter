package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpClient runs `gmeter mcp args...` with a client on the other end of its
// stdin and stdout, the way an agent runs it. It returns the client, what the
// server wrote to stderr, and a channel of its exit code once it stops.
func mcpClient(t *testing.T, args ...string) (*mcp.ClientSession, *bytes.Buffer, <-chan int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	var stderr bytes.Buffer
	code := make(chan int, 1)
	go func() { code <- runMCP(ctx, args, serverIn, serverOut, &stderr) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatalf("connecting to gmeter mcp: %v (stderr: %s)", err, stderr.String())
	}
	t.Cleanup(func() { cs.Close() })
	return cs, &stderr, code
}

func TestMCPServesTheToolsOverStdio(t *testing.T) {
	cs, _, _ := mcpClient(t)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "check_slo,compare_runs,find_capacity,load_test" {
		t.Errorf("tools = %s", got)
	}
}

func TestMCPFlagsSetWhatAnAgentMayDo(t *testing.T) {
	cs, _, _ := mcpClient(t, "-max-rate", "5", "-allow", "10.0.0.0/8,example.test")

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "load_test",
		Arguments: map[string]any{"url": "http://127.0.0.1:1", "rate": 50, "duration": "1s"}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if !res.IsError || !strings.Contains(text, "-max-rate") || !strings.Contains(text, "limit of 5") {
		t.Errorf("error=%v text=%q, want the -max-rate flag to have set the limit", res.IsError, text)
	}
}

func TestMCPStopsWhenItsInputCloses(t *testing.T) {
	cs, _, code := mcpClient(t)
	cs.Close()

	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("exit code %d, want 0", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop after the client went away")
	}
}

func TestMCPRefusesFlagsItCannotUse(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown":        {"-nope"},
		"negative rate":  {"-max-rate", "-1"},
		"stray argument": {"extra"},
		"bad allow":      {"-allow", "10.0.0.0/99"},
	} {
		var stderr bytes.Buffer
		if got := runMCP(context.Background(), args, strings.NewReader(""), io.Discard, &stderr); got != 2 {
			t.Errorf("%s: exit code %d, want 2 (stderr: %s)", name, got, stderr.String())
		}
	}
}

func TestMCPTellsTheOperatorWhatItAllowsOnStderrNotStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runMCP(context.Background(), []string{"-max-rate", "250"}, strings.NewReader(""), &stdout, &stderr)

	if !strings.Contains(stderr.String(), "loopback") || !strings.Contains(stderr.String(), "250") {
		t.Errorf("stderr %q, want the policy in force", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q: it carries the protocol, and nothing else may be written to it", stdout.String())
	}
}
