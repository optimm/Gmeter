# Replaying real traffic

Back to the [README](../README.md).

## Replaying real traffic (`gmeter replay`)

One URL at a steady rate is rarely what production sees. `gmeter replay` takes a log of real requests and sends
them, each one when it was recorded, so the real mix of endpoints and the real bursts reach the server. It is
still open loop: every request is due at its recorded time whatever the server is doing, and latency is
measured from that time.

```bash
gmeter replay -file traffic.jsonl http://staging:8080            # at the recorded pace
gmeter replay -file access.log -speed 2 http://staging:8080      # twice as fast as recorded
gmeter replay -file traffic.jsonl -rate 500 -duration 2m http://staging:8080   # ignore the times, 500 rps
```

The URL is only the server (scheme, host, and an optional path prefix such as `http://host/api`); the log's paths
are appended to it. A log can therefore never send a request to another host.

| Flag | Default | Meaning |
|---|---|---|
| `-file` | | the traffic to replay (required) |
| `-format` | `auto` | `jsonl`, `clf` (an access log) or `auto`: a first character of `{` means JSON Lines |
| `-speed` | `1` | recorded timing only: `2` replays twice as fast as recorded, `0.5` half as fast |
| `-rate` | | send at this constant rate instead of at the recorded times; the trace is cycled in order |
| `-duration` | one pass | with `-rate`: how long to keep sending; by default each request is sent once |
| `-methods` | `GET,HEAD` | which methods to replay, or `all` (see below) |
| `-endpoints` | `20` | the most rows in the per-endpoint breakdown, 2 to 100; `0` leaves it out (see below) |
| `-group` | | name the endpoint of the paths a rule matches, e.g. `/products/:slug` (repeatable; see [Per endpoint](#per-endpoint)) |
| `-arrivals` / `-seed` | `poisson` | access logs and `-rate` only; see below |

`-workers`, `-timeout`, `-spin`, `-H`, `-json`, `-quiet`, `-insecure`, `-cacert`, `-cert` and `-key` work as for a plain run. `-H` headers go with every
replayed request, which is how to supply credentials (an access log has no headers). Headers in a JSON Lines
trace are sent too and replace a `-H` of the same name. No headers or bodies are saved in the results. Exit code
is `0` when it ran, `1` if the report could not be written, and `2` for bad usage or an unreadable trace.

### Trace formats

**JSON Lines**: one object per line. Only `path` is required.

```json
{"at": 0.25, "method": "POST", "path": "/orders?id=7", "headers": {"Content-Type": "application/json"}, "body": "{\"qty\":2}"}
```

`at` is when the request was made, in seconds from any fixed point (only the differences matter): give it to every
request or to none. `status` is optional: what the server answered when the request was recorded, from 100 to 599,
so the replay can say how many responses differ (see [Status against the log](#status-against-the-log)). Unknown
fields are an error, so a misspelt `header` does not silently send the wrong thing. Bodies are text. Without `at`
the trace has no pace of its own, so `-rate` is required.

**Access log**: the common and combined log formats that nginx, Apache, HAProxy and most proxies write. Each line
gives a method, a path and the second it was logged in. Lines that are not usable requests (garbage, a `"-"`
request from a connection that sent nothing, a proxy-style absolute URL) are counted and reported, not fatal.
An access log has no request bodies, so a replayed `POST` has none. Each line's status is kept (a `-` or a number that
is not a status leaves that request with nothing to compare).

### What it does with the times

- **Recorded timing** (the default) sends request *i* at its recorded time, relative to the first one.
- **An access log only knows the second** a request was logged in. What survives is how many requests there were in
  each second, so Gmeter spreads them inside it: `-arrivals poisson` (the default) places them at random points,
  which is what independent clients look like given only a count, and `-seed` repeats it; `-arrivals even` spaces
  them exactly. Exact JSON Lines times need no spreading, so setting either flag for them is an error.
- **`-rate`** discards the times and sends the trace in order at a constant rate, cycling if the run is longer
  than the trace. `-arrivals` and `-seed` then work as in a plain run.

### It will not write to your server unless you say so

A replay sends whatever the log says. A log full of `POST`, `PUT` and `DELETE` would create, change and delete
things on the server under test, so the default `-methods GET,HEAD` leaves those requests out, says how many it
skipped, and records that in the JSON. Pass `-methods all` or a list such as `-methods GET,POST` when that is
what you want, against an environment where it is safe. The skipped requests' times go with them, so the
replayed load is the filtered load.

### What the pace changes

The same 4000 requests (10 seconds; every second 800ms at 200 rps and then a 200ms burst at 1200 rps, 400 rps on
average) against a toy server that handles about 800 rps:

| How the requests were sent | p50 | p99 |
|---|---|---|
| flat 400 rps, evenly spaced | 5.2ms | 5.4ms |
| flat 400 rps, Poisson | 5.4ms | 9.4ms |
| the same traffic as an access log (one-second times), spread as Poisson | 5.7ms | 13.6ms |
| the same traffic as JSON Lines, recorded times | 31.5ms | 121.1ms |

Single runs on a laptop that was also running other things, so trust the ordering and the rough size of the gaps, not the
exact numbers. The point is the last row: the average rate is far below what the server can do, and the p99 is
still more than 20 times the flat run's, because the bursts queue. `gmeter diff` notes when two runs replayed different
traces, speeds or timings, since they are not like-for-like.

### Per endpoint

A replay reports each endpoint on its own as well as the whole mix. An endpoint is the method and the path, with
the query string dropped and any path segment that is an identifier (only digits, a UUID, or 16 or more hex
characters) replaced by `:id`: `GET /orders/18273?ref=mail` and `GET /orders/99` are both `GET /orders/:id`. The
busiest endpoints get a row each and the rest are grouped as `(other)`; `-endpoints N` sets the most rows (each one
costs two histograms, three with phases, hence the limit of 100). Requests left out by `-methods` are not counted,
so the ranking is of what was sent.

Identifiers are recognised by how they look, so a slug (`/products/blue-shoes`) or a file name (`/img/a.png`) would
be an endpoint of its own each time. `-group` names them instead (repeatable; the first rule that matches wins):

```bash
gmeter replay -file access.log -group '/products/:slug' -group '/products/*/reviews' -group 'GET /static/**' http://staging:8080
```

`*` or `:name` stands for any one non-empty segment, a last `**` for whatever follows (nothing included), and any
other segment for itself, compared exactly. A method limits a rule to that method. The endpoint is named after the
rule (`GET /products/:slug`), and paths no rule matches are named by their shape as before. Put the more specific
rule first: `/products/:slug` before `/products/**`, or the second takes everything.

Real output: a 10-second trace at about 300 rps (65% `/orders/N`, 20% `/search?q=...`, 15% `/users/N/cart`)
against a toy server with four slots, where `/search` holds a slot for 40ms and everything else for 2ms. With
`-workers 50`:

```
endpoints, busiest first (corrected latency; svc p99 is service latency, from when each request was actually sent)
  endpoint             scheduled     bad  failed       p50       p99       max   svc p99
  GET /orders/:id           1961       0       0     4.3ms    72.9ms    89.0ms    72.7ms
  GET /search                625       0       0    42.6ms   105.3ms   118.9ms   103.0ms
  GET /users/:id/cart        459       0       0     4.3ms    70.6ms    80.5ms    70.5ms
```

`/orders` costs 2ms but has a p99 of 73ms. This load keeps about three of the server's four slots busy (300 rps
at about 10ms of slot time per request on average), so a request often has to wait for one, and a 2ms request
waits as long as a 40ms one. The service p99 is just as high, because that wait is inside the server and both
latencies include it. The same trace with `-workers 4` waits in the generator instead:

```
  GET /orders/:id           1961       0       0     8.0ms   109.6ms   131.4ms     5.6ms
  GET /search                625       0       0    46.7ms   140.8ms   157.4ms    44.2ms
  GET /users/:id/cart        459       0       0     6.5ms    83.3ms   124.4ms     5.1ms
```

Now the corrected p99 is 110ms and the service p99 is 5.6ms: once a request was sent it was answered quickly,
but it had to wait for a free worker first (the generator line reported 37% of dispatches late). A gap between
the two columns means requests waited in Gmeter; raise `-workers`.

With phases on (the default) a second table says where each endpoint's time went: the mean time per answered
request in each phase, so the columns add up to the mean. A fresh run of the same two setups:

```
-workers 50
  endpoint             answered      mean     queue   connect      send      wait  transfer
  GET /orders/:id          1961    12.8ms      15µs       6µs      12µs    12.8ms      12µs
  GET /search               625    50.9ms      17µs       4µs      12µs    50.8ms      13µs
  GET /users/:id/cart       459    11.9ms      15µs       1µs      12µs    11.8ms      12µs

-workers 4
  endpoint             answered      mean     queue   connect      send      wait  transfer
  GET /orders/:id          1961    15.6ms    13.0ms     498ns       9µs     2.6ms      12µs
  GET /search               625    53.8ms    12.9ms     936ns      10µs    40.9ms      14µs
  GET /users/:id/cart       459    14.7ms    12.1ms       1µs       9µs     2.6ms      12µs
```

A 2ms `/orders` request spends 12.8ms in `wait` with 50 workers: that is the server, holding it until a slot
was free. With 4 workers its `wait` is 2.6ms, about its own cost, and the other 13ms are `queue`: it waited in
Gmeter. All three endpoints queue for 12-13ms because the limit is the one shared pool of workers.

- **Means, plus the tail of `wait`.** A distribution costs about a quarter of a megabyte, and 100 endpoints with 7
  phases each would take hundreds, so each endpoint keeps one: its `wait`, the server's own time, shown as the last
  column (`wait p99`) and saved as `endpoints[].latency_wait`. That is the one a mean hides. A real run against a
  server where `/search` takes 5ms but one request in 30 takes 150ms, and `/orders/:id` always takes 8ms, at 200 rps:

  ```
    endpoint         answered      mean     queue   connect      send      wait  transfer  wait p99
    GET /orders/:id      1500     9.5ms     136µs       1µs      36µs     9.3ms      39µs    12.8ms
    GET /search          1500    10.7ms     128µs       3µs      36µs    10.5ms      27µs   150.6ms
  ```

  Their mean waits are a millisecond apart, and their tails more than ten times apart. The whole-run table has
  every phase's percentiles. `diff` shows how a regressed endpoint's wait p99 moved (see below).
- **`connect` is dns + connect + tls**, averaged over all of the endpoint's answered requests, not only those
  that opened a connection. The JSON keeps every phase apart, with how many requests had it, its mean and its
  share (`endpoints[].phases`).
- **Timeouts and failed requests have no phases**, so with any, the columns add up to the mean of the answered
  requests only.

The names are paths from your trace, so they go into the saved JSON (`endpoints`). The query string never does,
but a secret in a path segment that is not an identifier would; `-endpoints 0` keeps every path out of the report
and the file.

### Status against the log

A log records what the server answered, so the replay can check that the target answers the same way. An access log
of 600 requests that were all `200`, replayed against a staging that has no orders with an id divisible by 4 (real
output):

```
requests   600 scheduled   480 ok   120 bad status   0 failed   0 dropped
status     200: 480   404: 120
vs log     600 compared, 120 differ (20.0%): 2xx→4xx 120
warning    120 of 600 responses (20%) were of a different class than the log recorded (2xx→4xx 120): the target may
           not hold the data the log asked for, so these latencies may not be what production would see; check
           before reading them
...
endpoints, status vs the log (responses of another class than the log's)
  endpoint               differ    share
  GET /orders/:id    120 of 480    25.0%
```

- **By class.** A `204` where the log has a `200` is the same answer; a `404` is not. Changes are named by class and
  listed most common first (`2xx→4xx`), up to four, then a count of the rest.
- **Only requests with a logged status are compared**, and only the ones that got a response: a timeout or a refused
  connection has no status to compare. A JSON Lines trace may give `status` to some requests and not others.
- **The warning is at 5%** of the compared responses. A few differing are normal (a record deleted since); the `vs log`
  line shows the exact figure either way, and the section under it lists the endpoints that differ, the largest share
  first, so a missing set of ids shows up as one endpoint and not as a vague overall figure.
- **It does not change the latency.** The `404`s are still in the percentiles. The line says how much of the run they
  are; reading it is up to you.
- Saved in the JSON as `vs_log` (`compared`, `different`, and `changes` with `logged`, `got` and `count`), and for
  each endpoint as `log_compared` and `log_different`; the keys are left out when nothing was compared.

### How far to trust a replay

- **One-second logs hide short bursts.** The access-log row above recovers some of the burstiness (13.6ms against
  5.4ms) but not the 200ms bursts, which are only visible in the millisecond times of the JSON Lines trace. If the
  bursts matter, record the times at millisecond resolution.
- **Logged times are completion times.** A server writes the line when the request finishes, so the time is when
  it was answered, not when it arrived. Lines can also be slightly out of order, so the trace is sorted.
- **The target may not answer as the log's server did.** A replayed id that does not exist in the target returns
  `404`, usually faster than a real answer would be. The report counts those (see
  [Status against the log](#status-against-the-log)) and warns at 5%.
- **Cycling warms caches.** With `-rate`, a trace shorter than the run is sent over and over, and later passes
  are served from warmer caches than the first. The report says so when it happens (`the trace has 3 requests but
  the run sent 40, so it was replayed 13.3 times: ...`), and the warning is saved in the JSON. Use a trace at least
  as long as the run.
- **An endpoint's latency includes what it waited behind.** Requests share the server's threads and connections,
  so a fast endpoint that arrives while slow ones hold them all is slow too (the 2ms `/orders` with a p99 of 73ms
  above). The table cannot tell "slow itself" from "stuck behind a slow neighbour"; to separate them, replay that
  endpoint alone from a trace of only its requests. A burst larger than `-workers` in flight shows up as late
  dispatches in the generator line.
- **Endpoint names are recognised by shape unless you say otherwise.** Identifiers are found by how they look, so
  a slug such as `/products/blue-shoes` or a name such as `/img/123.png` stays a different endpoint each time
  unless a `-group` rule names it, and once there are more than `-endpoints` rows the least busy fall into
  `(other)`.
- **The whole trace is read into memory before the run starts**, because it has to be sorted by time (an access log
  is in completion order) and its endpoints ranked before the first request goes out. Measured with `/usr/bin/time
  -l` on a 1,000,000-line combined-format access log (147MB, 2,000 requests a second): about 140 bytes per request
  stay in memory, and the process peaked at 470MB. Before this was measured, each request kept its whole log line
  alive and the runner copied the trace, which peaked at 1.04GB. Memory grows with the number of requests, so a
  10-million-line log needs a few GB: replay a slice of it (`head -n 2000000`), which is usually more than a test
  needs anyway.
