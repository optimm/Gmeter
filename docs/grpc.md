# gRPC

Back to the [README](../README.md).

## gRPC (`gmeter grpc`)

```bash
# a server with server reflection, without TLS
gmeter grpc -plaintext -call shop.Orders/Get -data '{"id":"42"}' -rate 1000 -duration 30s orders:9090

# a server without reflection: give it the schema
protoc --descriptor_set_out=orders.protoset --include_imports orders.proto
gmeter grpc -protoset orders.protoset -call shop.Orders/Get -data @get.json \
  -H 'authorization: Bearer ...' orders.internal:443
```

`gmeter grpc` calls one unary method on the same open-loop schedule as an HTTP run: latency from each call's
intended start, `-rate`/`-duration` or `-stages`, Poisson arrivals, the timeline, the generator line, `-json`
and `diff`. The demo service in `internal/grpcdemo` (5ms of work a call), 1000 calls a second for 10 seconds:

```
$ gmeter grpc -plaintext -call gmeter.demo.Echo/Say -data '{"name":"ann","delay_ms":5}' -rate 1000 -duration 10s 127.0.0.1:18097
gmeter: gRPC /gmeter.demo.Echo/Say on 127.0.0.1:18097 at 1000 rps for 10s, over 1 connection, up to 50 in flight
requests   10000 scheduled   10000 ok   0 bad status   0 failed   0 dropped
achieved   999.4 rps (target 1000) over 10.01s
codes      OK: 10000
generator  timer lag p99 417ns, max 1.3ms   late 1 (0.0%)

latency, corrected (from each request's intended send time)
  p50 5.2ms   p90 5.3ms   p99 5.3ms   p99.9 6.2ms   max 8.3ms
latency, service (from when the request was actually sent)
  p50 5.2ms   p90 5.3ms   p99 5.3ms   p99.9 5.7ms   max 7.9ms
```

| Flag | Default | Meaning |
|---|---|---|
| `-call` | | the method, `package.Service/Method` (required); unary only |
| `-data` | empty message | the request as JSON in the protobuf mapping; `@file` reads it from a file |
| `-protoset` | | the schema as a `FileDescriptorSet`; without it the server is asked by server reflection (v1, or v1alpha) |
| `-plaintext` | `false` | no TLS (HTTP/2 over cleartext); otherwise TLS, verified against the system's roots |
| `-insecure`, `-cacert` | | as for HTTP: skip verification, or trust these authorities |
| `-cert`, `-key` | | as for HTTP: the client certificate for mutual TLS |
| `-H` | | metadata `"key: value"` sent with every call (repeatable); keys starting `grpc-` are reserved and refused |
| `-conns` | `1` | connections to spread the calls over, in turn |

`-rate`, `-duration`, `-stages`, `-arrivals`, `-seed`, `-workers` (calls in flight), `-timeout`, `-spin`, `-json` and
`-quiet` work as for a run.

How it differs from an HTTP run:

- **Answers are counted by status code**: `codes      OK: 10000`, or `codes      RESOURCE_EXHAUSTED: 200`. `OK` is
  ok and every other code is a bad status, timed like any answer. The JSON saves `codes`, `run.protocol` is
  `gRPC` and `run.method` the full method name, so `diff` notes when it is comparing a gRPC run with an HTTP one.
- **`UNAVAILABLE` is counted as a failure, not an answer.** gRPC uses the same code for a connection that broke
  and for a server that says it cannot serve right now, and the two cannot be told apart from the client. Like a
  refused connection, it is not timed. A call that runs past `-timeout` is a timeout and is timed from its due
  time; `DEADLINE_EXCEEDED` sent by the server itself is an answer.
- **One request, encoded once.** `-data` is parsed against the method's input type, a misspelt field is an error
  instead of an empty value, and the same bytes are sent on every call. Answers are not decoded: the status is
  what is measured. There is no trace replay for gRPC.
- **Nothing is called until everything is checked.** The connections are opened before the clock starts (so
  connecting is not in the latencies), and the schema is read and the request built; connecting and reading the
  schema may take up to `-timeout`. Whatever stops that is said, with what to do:

  ```
  gmeter grpc: gmeter.demo.Echo has no method Shout; it has: Say, Chat
  gmeter grpc: the request is not a gmeter.demo.SayRequest: proto: (line 1:2): unknown field "nme"
  gmeter grpc: cannot connect to 127.0.0.1:18097: TLS handshake: tls: first record does not look like a TLS handshake
    if the server does not use TLS, try -plaintext
  ```
- **`-conns` is not usually what limits the test.** gRPC sends every call of a connection over it at once. Against
  the demo service with no work per call, 64 in flight, one connection kept 40000 calls a second with a corrected
  p99 of 392µs, and four connections did no better (516µs); at 20000 a second it was 272µs against 328µs (one run
  each). More connections help when the server limits the calls in flight per connection, or when a load balancer
  in front spreads connections and not calls.
- **Not offered:** streaming methods, the per-phase breakdown (`-phases` uses HTTP's hooks), `-agents`, and `find`.
