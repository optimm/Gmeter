# Several generators

Back to the [README](../README.md).

## Several generators (`-agents`)

One machine can only send so much. When the generator runs out of CPU, its requests go out late, and the late
dispatches are counted toward latency (that is the point of measuring from the intended send time), so the report
blames the server for the generator's limit. Gmeter says when this happens (the `late` warning in the
[generator line](reading-the-report.md#reading-the-generator-line)), and `-agents` is what to do about it: run `gmeter agent` on a few
machines, and let one controller split the test among them.

```bash
# on each generator machine: the same gmeter build, the same password
export GMETER_TOKEN='a long random password'      # at least 16 characters
gmeter agent -listen 0.0.0.0:7070                  # the default, 127.0.0.1:7070, is reachable only from that machine

# on the controller, with the same GMETER_TOKEN
gmeter -agents gen1:7070,gen2:7070,gen3:7070 -rate 60000 -duration 60s -workers 192 https://staging/api
gmeter replay -agents gen1:7070,gen2:7070 -file access.log https://staging
```

Each agent sends a share of the load, and the report (and `-json`, `diff`) is of all of them together: the
agents send back their full latency histograms and the controller adds them up, so the merged p99 is the p99 of
every request, not an average of the agents' p99s. The password is read from `GMETER_TOKEN` on both sides, never
from a flag, which anyone on the machine could read in the process list.

Measured on one 10-core Mac, against a local server that answers at once, 40000 rps for 10 seconds with
`-workers 64`. Each generator was held to one CPU with `GOMAXPROCS=1`, to stand in for a machine that is too
small for the test:

| generator | achieved | corrected p50 | corrected p99 | late |
|---|---|---|---|---|
| no CPU limit (shows what the server can do) | 39992 rps | 112µs | 364µs | 0.5% |
| one CPU | 33265 rps, 33219 rps | 1.02s, 1.06s | 2.00s, 2.02s | 100% |
| four agents with one CPU each | 39998 rps, 39998 rps | 153µs, 153µs | 579µs, 4.4ms | 0.6%, 1.0% |

One CPU could not keep up: every request went out late, and the corrected p99 of 2 seconds is the generator's
backlog, not the server. Four agents with one CPU each sent the full rate, and their merged result is close to what
the unlimited generator measured. The two four-agent p99s (579µs and 4.4ms) show how much the tail moves between
runs on a laptop that is also running the server, the controller and everything else. `gmeter diff` of the
one-CPU run against the four-agent one starts with `note  load generators differ (A: this machine, B: 4 agents)`.

How the load is shared:

- **A rate or a profile** is split evenly: each of *n* agents runs every stage at 1/*n* of its rate. With even
  arrivals each agent is offset by a fraction of the gap between requests, so together they send exactly the
  one-machine schedule, not *n* requests at once. With Poisson arrivals each agent gets its own seed (`-seed`,
  `-seed`+1, ...), and the sum is a Poisson process at the full rate. It is not the same arrival times as one
  machine with that seed.
- **A recorded replay** deals the requests out in turn, each at its recorded time, and every agent's run lasts as
  long as the whole trace. **A replay at a fixed rate** is interleaved so that together the agents send the trace
  in its order, cycling as one machine would.
- **`-workers` is the total**, divided among the agents; each needs at least one.

What to know before relying on it:

- **Nothing is sent unless every agent can run its share.** The controller checks each share itself, then asks
  every agent whether it answers, has the password, speaks the same protocol version (run the same build
  everywhere) and is free. If any is not, no agent is sent anything, and the error names the agent.
- **The agents start together, give or take the time their shares take to reach them.** Each starts 2 seconds
  after its share arrives, so their clocks do not need to agree, but a share that arrives late starts late. A
  large replayed trace takes longer to send. The timeline's windows are lined up by number, so they are only as
  aligned as the starts were.
- **There is no live progress**, and a run that fails or is interrupted has no report: if one agent fails, the
  others are stopped and the first error is printed; Ctrl-C stops every agent. A part of a test is not the test.
- **The generator line is the agents' together.** `late` adds them up, so a warning says some generator was
  short, not which. Run the one-agent test on that machine to find out.
- **The password is full control of the agent's host.** Anyone who can reach an agent and knows the password can make
  it send requests anywhere, with any method, headers and body the caller chooses. Treat it like a shell on that
  machine: keep it out of logs and repositories. The password and the traffic are not encrypted either, so run agents
  on your own machines, on a network you trust; an agent listens only on 127.0.0.1 unless told otherwise, and says so
  when it is reachable from the network.
- **Unix sockets are opt-in on the agent.** A controller can set `-unix-socket` (or `-connect-to`) on all its own
  shares, but an agent refuses a job that names a `unix_socket` path unless it was started with
  `-allow-unix-socket`: the caller could otherwise dial `docker.sock` or any other local service with a
  controller-chosen method and body. Only enable it when the controllers are as trusted as the host itself; when
  enabled, the agent says so at startup.
- **`gmeter find` runs on one machine.** Each of its trials is short, and a capacity search is usually limited by
  the server; `-agents` is for a run and a replay.
