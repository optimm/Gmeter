# How it works

Back to the [README](../README.md).

## How it works

```
 pacer.Schedule ──▶ scheduler goroutine ──▶ chan time.Time ──▶ N worker goroutines
 (At(n) = start      sleeps until each        (intended send     one request at a time,
  + n/rate, no        request is due; blocks   times, buffer N)   own Recorders, no locks
  drift)              if every worker is busy                          │
                                  ▲                                    ▼
          sampler goroutine ─────┴── pulls atomic snapshots ◀── Recorders (atomic, padded)
          diffs consecutive snapshots into per-window stats
```

- **`internal/pacer`**: the schedule is computed from the request index, never by accumulating an interval,
  so rounding error does not build up over millions of requests. If the generator falls behind it catches up
  by sending back-to-back, and those requests still carry their original due time. A profile is a list of
  constant or linearly ramping stages, and the k-th request is due when the expected cumulative count reaches
  k (even arrivals) or the k-th point of a seeded unit-rate Poisson process (poisson), found by inverting that
  count in closed form. `WaitUntil` is the hybrid sleep-then-spin wait described above.
- **`internal/hist`**: a fixed-memory (~250KB) log-linear histogram in the style of HdrHistogram. Relative
  error is at most 2^-10 (about 0.1%) at any magnitude.
  - `Histogram` is the plain version, used for merged results.
  - `Recorder` has atomic fields, so one worker can record while the sampler goroutine snapshots it, with no
    lock on the hot path. `Histogram.Delta` turns two cumulative snapshots into "what happened in between".
  - The sampler goroutine **pulls** instead of workers pushing: during a stall the workers are blocked
    inside a request and could not push anything, which is exactly when the display has to keep updating.
  - `Dump` / `FromDump` save and restore a histogram as sparse JSON, so saved runs can be compared exactly
    (and, later, merged).
- **False sharing, measured.** Recorders are padded to 128-byte cache lines. On a 10-core Apple Silicon Mac,
  8 goroutines each recording into their own Recorder took 2.0-3.1 ns/op unpadded and 0.92-0.93 ns/op padded
  (five runs each). Reproduce with `go test -run '^$' -bench RecorderRecordParallel -cpu 8 ./internal/hist`.
- **`internal/replay`**: reads a traffic log (JSON Lines or an access log) into requests and, if it has times,
  into the offsets at which each is due. The runner takes those as `Offsets` instead of a profile, and the
  requests as `Requests`, cycled by schedule index. Access-log seconds are spread before the schedule is built,
  so the scheduler never needs to know where its times came from. It also names each request's endpoint and caps
  how many are reported; the runner only sees opaque labels.
- **`internal/runner`**: scheduler, workers, an HTTP/1.1 client capped at `-workers` connections, live
  counters (atomics), per-stage and per-label results (a small lock per stage and per label, taken once per
  response), the generator-health measurements, and the per-request phase clock (`httptrace` hooks behind a
  mutex, since the transport can fire one after the request returned). A label keeps phase totals, a time and a
  count per phase, and not histograms, which is what lets a hundred labels have phases.
- **`internal/report`**: text and JSON output, the per-window timeline, the per-endpoint tables, and run
  comparison (`diff`), including per endpoint and per phase.
- **`internal/grpcload`**: `gmeter grpc`'s calls. It reads the schema (server reflection, either version, following
  the imports file by file, or a descriptor set), builds the request with `dynamicpb` once, and sends those bytes
  through a codec that neither encodes nor decodes, using unary `Invoke` or streaming `NewStream`/`SendMsg`/`RecvMsg`.
  One scheduled call is one whole stream. The runner calls it through `Config.Call`, which replaces the
  HTTP request with any call that returns a status code name. `internal/grpcdemo` is an Echo service built from a
  hand-written descriptor, so the tests need neither `protoc` nor generated code. gRPC support is the only part
  with dependencies: `google.golang.org/grpc` v1.85.0 and `google.golang.org/protobuf` v1.36.12, pinned.
- **`internal/agent`**: `gmeter agent` and the controller. A job is a runner configuration that can be sent as
  JSON; `Split` cuts one into shares, and `Distribute` checks every agent, runs the shares at once and merges the
  results with `runner.Merge`, which adds histograms (every window keeps its own) and stage rates and never
  combines percentiles. The protocol is HTTP with a bearer password, versioned so that two different builds refuse
  to work together instead of disagreeing about what a job means. The password is full control of the agent host;
  jobs that ask it to dial a Unix socket are refused unless the agent was started with `-allow-unix-socket`.

### Choices worth knowing

- Latency is recorded for every response (any status) and for timeouts. Other transport failures
  (connection refused, DNS, reset, TLS) are counted but not recorded as latency, because a fast failure says
  nothing about how quickly the server answers. Recording timeouts matters: dropping them would make a hung
  server look fast.
- If the server is so slow that the backlog cannot drain, the scheduler gives up on undispatched requests once
  the run is overdue by one `-timeout`, and reports them as `dropped`.
- A late request can mean either a slow server filling the workers or a starved generator, so Gmeter does not call
  a run `generator.limited` on late dispatches alone. It also requires timer-lag p99 over 5ms, or a mean queue
  (`corrected - service`) over `4 × workers × mean service time`: W workers explain about W service times of
  queueing, and much more than that is time the scheduler lost. The rule is pure arithmetic on measured means, so
  it needs no timing-sensitive test, and a result's `find` verdict uses the measured stage rather than the warm-up.
- Precision costs CPU: the default `-spin 200µs` busy-waits, up to about a quarter of a core and only below about
  5000 rps. Turn it off with `-spin 0` if the generator shares a small machine with the thing it is testing.
- Redirects are not followed (a 302 is a result) and proxy environment variables are ignored.
- HTTP/1.1 by default; `-http2` for HTTP/2, never a mix of both in one run.

## Development

```bash
go test -race ./...
```

The tests include one that proves the correction (one connection, the server freezes for 400ms on the first
request, and the corrected p90 must show the queueing while the service p90 stays small) and one that proves
the live windows pinpoint when a stall happened.
