# Finding the highest rate that meets a target

Back to the [README](../README.md).

## Finding the highest rate that meets a target (`gmeter find`)

```bash
gmeter find -slo p99=50ms http://localhost:8080/health
```

It answers "how much can this take while still meeting my latency target?" without you guessing rates. The target
is a corrected-latency quantile and a limit (`p99=50ms`, `p99.9=200ms`, `p50=5ms`), plus an error budget
(`-max-errors`, default 1% of requests failing or returning a bad status: a server that answers 500 instantly has
superb latency and is not meeting anything).

The search starts at `-start` (100 rps) and:

1. **grows**: doubles the rate until a trial misses the target (or `-max` passes);
2. **descends**: halves it instead if the very first trial already fails, down to 1 rps;
3. **bisects**: narrows the gap between the highest pass and the lowest fail until it is within `-tolerance` (5%);
4. **confirms**: repeats the answer `-confirm` times (2) to see whether it holds.

Each trial runs a `-warmup` (2s) that is not judged, to take the cold-start cost of new connections and caches,
then `-trial` (10s) that is. Against the fixed-capacity server from above (4 requests at a time, 5ms each, so
about 800 rps), `-trial 5s -warmup 1s`, with the default Poisson arrivals:

```
find  GET http://127.0.0.1:18082/   target p99 ≤ 50ms, errors ≤ 1%   poisson arrivals (seed 11)   5s trials after a 1s warm-up

   #  phase         rate        p99  errors  result
   1  grow           100      8.2ms    0.0%  pass
   2  grow           200      7.7ms    0.0%  pass
   3  grow           400      9.1ms    0.0%  pass
   4  grow           800    183.1ms    0.0%  FAIL  p99 is 183.1ms, over the 50ms target
   5  bisect         600     12.7ms    0.0%  pass
   6  bisect         700     26.8ms    0.0%  pass
   7  bisect         750     52.9ms    0.0%  FAIL  p99 is 52.9ms, over the 50ms target
   8  bisect         725     34.5ms    0.0%  pass
   9  confirm        725     39.1ms    0.0%  pass
  10  confirm        725     38.8ms    0.0%  pass

result  the highest rate meeting p99 ≤ 50ms is about 725 rps
        the next rate up that was tried, 750 rps, failed
        held in 2 of 2 re-runs at 725 rps
```

That took 70 seconds: each trial costs its warm-up and its measurement plus a one-second pause so that one trial's
aftermath does not leak into the next. Progress lines go to stderr as it runs; `-json` saves every trial.

### Where the time grew

Each trial records the [phases](reading-the-report.md#where-the-time-goes) of its measured part (`-phases=false` turns that
off). After the answer, the report compares the trial that made it the answer with the first rate above it that
failed: the mean time per answered request in each phase, and which phase grew most. Two real runs against the same
800 rps server, on a busier machine than the table above (so lower answers):

```
where the time grew from 700 rps (pass) to 725 rps (FAIL), mean per answered request
  queue +259µs   connect +5µs   send +18µs   wait +37.7ms   transfer +785ns
  most of it is wait: the server (and the network) took longer to answer
```

The same search with `-workers 4`, so that Gmeter rather than the server runs out of room first:

```
where the time grew from 500 rps (pass) to 525 rps (FAIL), mean per answered request
  queue +92.8ms   connect 0ns   send +11µs   wait +1.2ms   transfer +23µs
  most of it is queue: requests waited inside Gmeter, for a free worker or a late timer, before being sent.
  The server's own time grew less, so this may be Gmeter's limit rather than the server's: run again with
  more -workers, on a quieter machine, to tell which
```

The second answer, 500 rps, is lower than the server's real limit, and the phases are what show it. When the
average number of requests in flight (rate × time per request once sent, by Little's law) exceeds `-workers`, the
report says that too. These are means, while the target is a quantile: a failure that sits only in the tail can
show no phase growing, and the report says so rather than naming one. `-json` keeps every trial's phases as
`phases` (`queue_ms`, `connect_ms` for dns + connect + tls, `send_ms`, `wait_ms`, `transfer_ms`).

### How far to trust the answer

The report says plainly when the answer needs a caveat, and these are the ones to look for:

- **unstable**: the confirmations disagreed. The same server with evenly spaced arrivals (`-arrivals even`) is the
  example: p99 stayed near 5.5ms up to 750 rps, so the search climbed to 775 rps, and there the two re-runs split,
  31.3ms and 53.1ms. It reported 775 rps with "held in only 1 of 2 re-runs, so treat it as an upper estimate"
  rather than presenting a cliff edge as a measurement. Poisson arrivals gave a steadier 725 rps.
- **at least N rps**: the search stopped at `-max`, not because the server gave out. Raise `-max`.
- **generator limited**: a trial whose latency looked fine, but where Gmeter could not keep time, is not trusted:
  it counts as a failure and the report says the server may sustain more. That means timer lag p99 over 5ms. A
  saturated generator that has stopped sleeping is not detected by this signal and only gets the late-dispatch
  warning; see
  [Reading the generator line](reading-the-report.md#reading-the-generator-line)). Run Gmeter somewhere quieter,
  or on a bigger machine.
- **no rate down to 1 rps met the target**: exit code 1.
- **interrupted** (Ctrl-C): the best rate found so far, and exit code 1.

It assumes what every load test assumes, that latency does not improve as the rate rises, and a noisy server breaks
that now and then; the confirmations are the check. A p99 from a 5-second trial rests on a few dozen requests, so
use the 10-second default or longer for anything you will act on. `-workers` (default 200 here) should be at
least rate times latency: with fewer, requests queue inside Gmeter instead of reaching the server, which the
corrected latency still counts, but a server that slows down under concurrency then sees less of it than real
clients would give it, and the answer can be optimistic. Exit codes: `0` a rate was found, `1` none was (or it was
interrupted), `2` bad usage.

| Flag | Default | Meaning |
|---|---|---|
| `-slo` | required | `p99=50ms`: a latency quantile and its limit, on the corrected latency |
| `-max-errors` | `1` | percent of requests that may fail or return a bad status |
| `-start` / `-max` | `100` / `100000` | where the search starts, and the most it will try (rps) |
| `-tolerance` | `5` | stop narrowing within this many percent |
| `-trial` / `-warmup` | `10s` / `2s` | measured time per trial, and the unjudged warm-up before it |
| `-confirm` | `2` | repeats of the answer at the end |

`-workers`, `-timeout`, `-method`, `-H`, `-body`, `-spin`, `-arrivals` (default `poisson` here), `-seed`, `-json`,
`-quiet`, `-phases`, `-insecure`, `-cacert`, `-cert` and `-key` work as for a plain run. Each trial uses the seed plus its number, so a printed seed reproduces the search.

### Journeys

With `-scenario FILE`, the search runs a [journey](journeys.md) instead: `-slo` then searches over **session
rate** — the trials and the report all say `sessions/s`. Each trial runs warm-up and measured session-rate
stages with `-trial`/`-warmup` as usual. The error rate includes check, extraction and incomplete-journey
failures; a trial with too few completed sessions to judge is a failure, not a pass.
