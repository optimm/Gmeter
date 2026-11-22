# Using Gmeter from Go

Back to the [README](../README.md).

## Using it from Go

The engine is a Go package, so a test, a benchmark or a deploy check can run a load and assert on it, with no process
to start and no output to parse.

```
go get github.com/optimm/Gmeter
```

```go
res, err := gmeter.Run(ctx, gmeter.Config{
	URL:  srv.URL,
	Rate: 500, Duration: 10 * time.Second,
	Requests: []gmeter.Request{{Path: "/cart"}, {Method: "POST", Path: "/checkout", Body: []byte(`{}`)}},
})
if err != nil {
	t.Fatal(err) // the Config could not run at all
}
if err := res.Meets(gmeter.SLO{Quantile: 0.99, Latency: 80 * time.Millisecond, MaxErrorRate: 0.001}); err != nil {
	t.Error(err) // e.g. "target missed: errors 0.4% over the 0.1% budget; p99 is 212ms, over the 80ms target"
}
```

`Run` takes what the flags take: a `Rate` for a `Duration`, or `Stages` for steps and ramps; `Poisson` and `Seed`; a set of
`Requests` sent in turn; `Header`, `Body`, `Workers`, `Timeout`; `TLS` (a `*tls.Config`, which is also how to give
a client certificate or skip verification); `HTTP2`; `UnixSocket` to connect to a Unix domain socket instead of the
host in `URL`; `ConnectTo` to dial another `host:port` while keeping the URL's host for the `Host` header and TLS;
and `OnRequest`, called with every request as it ends. The
`Result` has the counts (`Scheduled`, `OK`, `BadStatus`, `Failed`, `ErrorRate`, `Statuses`, `Errors`), the latency at any
quantile by both clocks (`Corrected(0.99)` from when a request was due, `Service(0.99)` from when it was sent), and
`WriteText`, `WriteJSON` and `WritePrometheus` for the report the command prints, the file `gmeter diff` compares,
and the metrics a Prometheus scrape can read.

`Meets` is the check. It fails when the latency at the target's quantile is over the limit, when more requests failed
than the error budget allows, when requests were left unsent, and when Gmeter could not keep time itself, because then
a good-looking result would be the generator's and not the server's. The error wraps `gmeter.ErrTargetMissed` and says
which part was missed. A target can also be read from the command line's spelling with `gmeter.ParseSLO("p99=50ms")`.

`FindCapacity` is `gmeter find` as a function: it takes a `Config` with no load, a target and `SearchOptions`, and returns
the highest rate that met the target, the lowest that did not, whether the generator may be the limit, and every trial.

```go
got, err := gmeter.FindCapacity(ctx, gmeter.Config{URL: srv.URL}, gmeter.SLO{Quantile: 0.99, Latency: 50 * time.Millisecond},
	gmeter.SearchOptions{Start: 50, Max: 2000})
fmt.Printf("holds %.0f requests per second (at least: %v)\n", got.MaxRPS, got.LowerBound)
```

To load-test a unary gRPC method, set `GRPC` and give the server's `host:port` as the `URL`. Everything else works the same:
the stages, `Poisson`, `Workers`, `TLS` (for a server that wants a client certificate too), `OnRequest`, `Meets` and
`FindCapacity`.

```go
res, err := gmeter.Run(ctx, gmeter.Config{
	URL:  "orders:9090",
	Rate: 1000, Duration: 30 * time.Second,
	GRPC: &gmeter.GRPC{Method: "shop.Orders/Get", Data: []byte(`{"id":"42"}`), Plaintext: true},
})
fmt.Println(res.Codes()) // map[NOT_FOUND:3 OK:29997]: the answers by status code; anything but OK counts as an error
```

`GRPC` takes what the `grpc` command's flags take: `Protoset` (a descriptor set, or the server is asked by reflection),
`Conns`, `Metadata`, and `Plaintext`. `Method`, `Header`, `Body`, `Requests` and `HTTP2` are for HTTP and are refused
alongside it. A `Sample` has the call's `Code` where an HTTP one has its `Status`. As in the command, `UNAVAILABLE` counts
as a failure (`Errors()["unavailable"]`), since gRPC says it both for a broken connection and for a server that is down.

The saved-results file, `plot`, agents and `replay` of a trace file stay in the command.
