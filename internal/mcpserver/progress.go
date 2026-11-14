package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// progressEvery is how often a long call tells the client it is still going.
const progressEvery = 500 * time.Millisecond

// reportProgress tells the client how far a call has got, if it asked to be told
// (by sending a progress token). total is how long the call is expected to take,
// or zero if that is not known. Call the returned function when the call is over:
// it stops the reports and waits for the last one to be sent.
func reportProgress(ctx context.Context, req *mcp.CallToolRequest, what string, total time.Duration) (stop func()) {
	if req == nil || req.Params == nil || req.Session == nil || req.Params.GetProgressToken() == nil {
		return func() {}
	}
	token := req.Params.GetProgressToken()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		tick := time.NewTicker(progressEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				elapsed := time.Since(started)
				p := &mcp.ProgressNotificationParams{ProgressToken: token, Progress: elapsed.Seconds()}
				if total > 0 {
					p.Progress = min(elapsed, total).Seconds()
					p.Total = total.Seconds()
					p.Message = fmt.Sprintf("%s: %.1fs of %.0fs", what, p.Progress, p.Total)
				} else {
					p.Message = fmt.Sprintf("%s: %.0fs so far", what, p.Progress)
				}
				req.Session.NotifyProgress(ctx, p)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
