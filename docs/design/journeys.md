# Stateful journeys: design proposal

Back to the [README](../../README.md). This is a design document only. Nothing here is implemented yet.

## The question

Replay answers "what did the traffic we recorded do to this service?". A flat rate answers "what happens at N
requests per second?". Neither can answer "can the service take 20 concurrent shoppers signing in, adding to a cart
and checking out?", because that question has state: step two needs a token from step one, and a slow step one delays
step two for that shopper.

A journey is a small, declared script for one session. Gmeter starts sessions on the same open-loop clock it uses for
requests today, and runs each session's steps in order. Other sessions start whether or not earlier ones finished, so
the test stays open loop. The session is the unit of arrival; the steps are the work it does.

## CLI shape

A new subcommand keeps the existing commands unchanged:

```bash
gmeter journey -scenario checkout.yaml -rate 20 -duration 5m \
  -workers 100 -json checkout.json http://staging.example.com/
```

`-rate` is **sessions per second**, not requests per second. A three-step journey started at 20 sessions/s sends up to
60 requests/s once all sessions are in flight. The report gives both numbers. `gmeter find` gains the same
`-scenario` flag and searches over session rate; its target and report say "sessions/s" so a rate cannot be mistaken
for a request rate. `gmeter replay` stays as it is: it reads a traffic log and has no notion of a session.

The scenario file is validated and compiled before any request is sent. Unknown keys, unknown variables, bad JSONPath,
bad regexes and missing CSV columns are usage errors (exit 2), not run-time surprises.

## Scenario YAML

The file describes one session, not a whole test. Load flags stay on the command line so that CI can change the rate
without editing traffic. The schema is strict; the fields below are the first version.

```yaml
name: checkout

feeds:
  shoppers:
    file: shoppers.csv    # first row is the header
    select: round-robin   # or random, with seed from -seed and the session number
    # seed: 7             # only with select: random

steps:
  - name: sign-in
    method: POST
    path: /api/login
    headers:
      Content-Type: application/json
    body: '{"email":"{{feeds.shoppers.email}}","password":"{{feeds.shoppers.password}}"}'
    check:
      status: [200, 201]
      json:
        token: {exists: true}
    extract:
      token:
        from: json
        path: $.token

  - name: create-cart
    think: 200ms          # after the previous step finished, before this one is due
    method: POST
    path: /api/carts
    headers:
      Authorization: 'Bearer {{vars.token}}'
    body: '{}'
    check:
      status: 201
      header:
        Location: {matches: '^/api/carts/[0-9]+$'}
    extract:
      cart:
        from: header
        name: Location

  - name: place-order
    think: 500ms
    method: POST
    path: '{{vars.cart}}/orders'
    body: '{"sku":"{{feeds.shoppers.sku}}"}'
    check:
      status: 200
      body: {matches: '"order_id":[0-9]+'}
      latency: {max: 2s}  # optional per-step ceiling, judged on corrected latency
```

`path` is always relative to the base URL from the command line. An absolute URL in a step is refused, so templating
cannot move load to another host (and so the MCP policy check of the base URL cannot be bypassed). Headers and bodies
are rendered from a fixed context: session variables, the session's CSV row, and read-only run facts such as the seed.
There are no file, environment, network or arbitrary Go template functions. Safe encoding helpers such as `urlquery`
and `json` are available explicitly; values are never interpolated into a URL host or a header name.

Each session gets its own variable map and its own CSV row. There is deliberately no shared mutable state between
sessions: concurrency bugs in the scenario would be indistinguishable from server bugs in the latency numbers.

## Session lifecycle and arrival rate

1. Compile and validate the scenario, read each CSV once, and assign step labels (one per step, up to
   `runner.MaxLabels`; a larger scenario is a usage error because labels carry histograms).
2. The existing pacer produces session start times at `-rate`, with the existing even or Poisson arrivals, seed and
   spin. That makes the arrival process comparable with every other Gmeter run.
3. Each start time takes one worker. The worker runs that session's steps sequentially, one request in flight at a
   time. When the last step ends, the worker takes the next session. `-workers` is therefore both the maximum
   concurrent sessions and the maximum concurrent HTTP requests, matching today's meaning.
4. A step's optional `think` is an intentional wait after the previous step finished. It does not make the step late.
   If the generator wakes later than the think time, the extra lateness counts.
5. If a step fails a check, cannot extract a required value or cannot render the next step, the session ends at that
   step by default. An optional `on_failure: continue` keeps going for diagnosis, but the session is still counted as
   failed. Failures are never silently skipped.

`-agents` works unchanged: each agent gets a share of the session schedule and the results are merged by histogram.
CSV selection uses the schedule's global session number, not a per-process counter, so agents with a share of the rate
still pick the same rows a single generator would.

## Latency anchoring

The honest clock has to mean something different inside a session, or every step would be charged for all earlier
steps and a single stall would be counted three times.

- **Step 1** is due at the session's scheduled start time. Its corrected latency runs from that time, exactly as a
  normal Gmeter request does.
- **Step n > 1** is due at `previous step finished + think`. Its corrected latency runs from that moment, so it
  measures what that step and the generator's hand-off cost, not what the whole session has already waited through.
- **Service latency** for every step still runs from when the request was actually handed to the client. The gap
  between corrected and service is time lost inside Gmeter, including a late wake-up after a think time.
- **Journey latency** from the session's start to its final step is recorded separately. It includes all think times
  and all earlier steps, and is the number to use for end-to-end user experience.

This keeps the two questions separate: step p99 says which call got slow, journey p99 says how long a shopper waited.
Generator-health detection reuses the same rule as a flat run — timer lag, or a queue far larger than the workers'
service times can explain — but computes the queue per step from that step's corrected and service means.

## Checks, extraction and templating details

Checks run on the response that was received, before the next step is considered. A failed check does not erase the
latency sample: the server did take that long to answer. It adds a check failure to the session's error count.

- `status` accepts one status, a list or a list of ranges such as `[200, 400..499]`.
- `header` and `body` accept `exists`, `equals` and `matches` (a Go regular expression). Response bodies are buffered
  only up to a configured limit; a larger body is a check failure, not unbounded memory.
- `json` uses a deliberately small JSONPath subset: `$`, `.field`, `[n]` and arrays of those. No filters, no scripts.
  A missing path is a failure unless the extractor says `optional: true`.
- `extract` reads from a JSON path, a named header or a regex group over the body or a header. Values become strings
  in the session's variable map; a value beyond the size limit is refused. Extracted values are never printed, saved
  in the JSON report or echoed by an MCP tool: they may be session tokens.
- Missing variables and invalid rendered requests fail the step before anything is sent. Header values containing CR
  or LF are refused. A template that would exceed the request-size limit is refused.

Transport failures keep their existing names. Check and extraction failures get their own error kinds in `Errors`, so
a report can say whether the service answered slowly, answered wrongly or failed to give the next step what it needed.

## Reporting reuse

Journeys should not fork the report. Every step execution is recorded as one labelled request, and the existing tally
machinery already keeps the pieces that matter:

- `Result.Labels` / `report.Summary.Endpoints`: one row per step, with counts and corrected and service histograms.
  The JSON key stays `endpoints` for compatibility; the text report calls these rows "steps" when a scenario was used.
- `Result.Phases` and the per-label phase totals: where a step's time went (queue, connect, send, wait, transfer),
  including a per-step wait distribution.
- `Result.Stages`: load-profile steps and ramps, unchanged. Steps in the scenario are not load-profile stages.
- `Result.Timeline`: the wall-clock view that shows *when* a step got slow, which a per-step summary cannot.
- `Result.GeneratorHealth` and `generator.limited` / `generator.reason`: as above, computed over the measured trial.

Only the journey-specific facts need new, additive fields: scenario name, sessions started, sessions completed,
sessions aborted, and counts by failure kind. They go under a `journey` key in `RunInfo`/the summary with `omitempty`,
so old JSON files and `gmeter diff` keep working. `gmeter diff` can gate on the whole run with `-threshold`, or on a
single step with `-endpoint-threshold`, because the step rows already live in `Endpoints` (with the usual minimum
sample count). No new histogram layout is needed.

## Interaction with replay and capacity search

Replay and journeys are different inputs to the same runner, not two versions of one feature:

| | `gmeter replay` | `gmeter journey -scenario` |
|---|---|---|
| Source | what a server was asked, recorded in a log | what a user might do, declared in YAML |
| Timing | the log's times, or a fixed request rate | a new session at each scheduled time |
| State | none between requests | variables and a CSV row per session |
| Safety | defaults to GET and HEAD | writes must be declared in the scenario |

`gmeter find -scenario FILE` replaces the trial's load profile with warm-up and measured **session-rate** stages,
using the same search and confirmation rules. `find.Evaluate` judges the measured stage against the SLO, but the error
rate now includes check, extraction and incomplete-journey failures, and the generator check uses per-step means. A
trial with too few completed sessions to judge is a failure, not a pass.

`gmeter mcp` gets optional journey tools or inputs only after the scenario is registered by the operator (a
`-scenario-dir` of named YAML files); agents refer to a name rather than pasting YAML by default. Summaries returned by
the MCP tools carry the same journey fields and the same generator verdict as CLI runs. `compare_runs` and
`gmeter://runs/{run_id}` work on them without special cases.

## Policy limits for an agent

The current MCP policy checks the target, rate, duration, workers, timeout and stream messages. A journey needs a few
more ceilings, because a short script can multiply a modest session rate into a large write load:

- **Effective request rate.** `session rate × number of steps` is checked against `MaxRate`, not just the session
  rate. Think times do not reduce this ceiling: a burst of sessions can still have every step in flight.
- **Scenario source.** Named scenarios only unless the operator explicitly allows raw YAML. Cap the file size, step
  count, regex and template length, rendered request size and CSV size. Refuse unknown keys and absolute step paths.
- **Writes.** POST, PUT, PATCH and DELETE in a step are refused for an agent unless the operator enables write
  scenarios and the target is allowed. The refusal names the setting to change, as other policy refusals do.
- **Data.** Cap variable and extracted-value lengths, never return values in tool output or stored reports, and cap
  the number of CSV rows and columns read into memory. The existing cloud-metadata and loopback rules still apply to
  the base URL.
- **Time.** Cap total session length, including think times, and the number of bounded repeats if `repeat` is added
  later. A stalled session must not hold a worker forever; the per-request timeout still applies to each step.
- **Steps.** Reject a scenario with more steps than the label limit, and keep the same per-run session limit as the
  worker limit. A failed check can end a session early, but it cannot create new sessions or new targets.

## Out of scope for the first version

- Cross-step branching based on response content beyond declared checks and extracted values.
- Shared state or message queues between sessions.
- Loading scenarios from a URL, or extracting a scenario from a recorded trace.
- Replaying a CSV row across sessions with writes to the CSV; feeds are read-only.
- Changing the JSON schema version. The first version adds optional fields only.

## What would be built, in order

1. A `journey` compiler: strict YAML schema, validation, CSV reader, feed selection and template/extractor/check
   evaluation, with all limits injectable so tests use synthetic inputs rather than wall-clock timing.
2. A runner mode that schedules sessions and executes a compiled scenario step by step through the existing HTTP
   client and tallies, with the anchoring rules above and additive journey counts.
3. Reporting: step rows through the existing endpoints path, journey fields in JSON/text, and generator and SLO
   evaluation using per-step measurements.
4. CLI: `gmeter journey -scenario`, `gmeter find -scenario`, and docs for both; then MCP inputs and policy checks.
5. Tests: a local server with a stateful token/cart flow; synthetic results for check failures, extraction failures,
   anchoring and generator health; hold the load there and across several generators. No timing-sensitive assertions.
