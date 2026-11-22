# Journeys: stateful sessions of steps

Back to the [README](../README.md).

A journey is a small declared script for one session: a sequence of steps the
server may answer with different things at different times — log in, then add
to a cart, then check out. Other sessions start at the same open-loop clock,
so the test stays open loop: sessions are the unit of arrival.

`gmeter journey -scenario FILE` runs sessions:

```bash
gmeter journey -scenario checkout.yaml -rate 20 -duration 5m -workers 100 http://staging.example.com/
```

`-rate` is **sessions per second**, not requests per second. The report gives
both numbers: `sessions` counts sessions, the step rows are the journey's
steps, and `achieved` is sessions/s. `gmeter find` has the same `-scenario`
flag and searches over session rate (see [Finding the highest rate](find.md)).

## The scenario file

The file describes one session, strictly: unknown keys, bad durations,
invalid templates, bad checks and missing CSV columns are usage errors (exit
2) before any request is sent.

```yaml
name: checkout                  # required: the scenario's name
seed: 7                         # optional: random feed selection, with select: random

feeds:                          # optional: CSV rows, one per session
  shoppers:
    file: shoppers.csv          # first row is the header, relative to this file
    select: round-robin         # or random

steps:
  - name: sign-in               # required: the step's label (unique per scenario)
    method: POST                # default GET; other methods need -H's body style as you say
    path: /api/login            # relative to the base URL; absolute URLs are refused
    think: 200ms                # wait after the previous step finished, before this one
    headers:
      Content-Type: application/json
    body: '{"email":"{{vars.email}}"}'
    check:
      status: [200, 201]        # a status, a range, or a list of either ("lo..hi")
      header:
        Location: {exists: true}   # or {equals: /api/carts/1}, or {matches: '^/api/carts/[0-9]+$'}
      json:
        token: {exists: true}      # or {equals: abc}, or {matches: '^[a-z]+$'}
    extract:
      token: {from: json, path: $.token}
    on_failure: continue        # or nothing: the session ends at a failed step

  - name: create-cart
    path: /api/carts
    check:
      header:
        Location: {matches: '^/api/carts/[0-9]+$'}
    extract:
      cart: {from: header, path: Location, optional: true}

  - name: place-order
    path: '{{vars.cart}}/orders'
    check:
      status: 200
      body: {matches: '"order_id":[0-9]+'}
```

Checks run on the response before the next step is considered, and a failed
check does not erase the step's latency: the server did take that long to
answer. If a check fails, or an extraction fails, or a step cannot render,
the session ends at that step by default; `on_failure: continue` keeps going
for diagnosis, but the session is still counted as aborted and the failure is
counted in the journey's error figures.

Extraction sets session variables (`vars`) from the response; a missing value
fails the extraction unless it is `optional: true`. The path is a small
JSONPath subset: `$`, `.field`, `[n]`, e.g. `$.token` or `$[0].id`. From
`header`, a named header is read; from `regex`, a group over the body or,
with `header: Name`, a named header; `group` (default 1, or 0 for the whole
match) chooses the group.

## Templating

Headers and bodies (and paths) are text/template with a restricted language:
`{{vars.token}}` and `{{feeds.shoppers.email}}` are the data roots, plus the
session number as `{{session}}`. `missingkey=error`: a variable the session
does not have is a step failure, not a literal "<no value>". Safe encoding
helpers are available explicitly: `{{urlquery v}}` and `{{json v}}`. There are
no file, environment, network or arbitrary Go template functions, and header
names are never templated; values are never interpolated into the URL host,
since every step's path must start with `/` and is appended to the base URL.

## Latency anchoring

The honest clock measures each step from its own due time:

- **Step 1** is due at the session's scheduled start, like a normal request.
- **Step n > 1** is due at `previous step finished + think`. Its corrected
  latency runs from that moment, so it measures the step and the
  generator's hand-off, not the whole session's wait.
- **Service latency** of every step runs from when the request was actually
  sent. The gap between corrected and service is time lost inside Gmeter,
  including a late wake-up after a `think`.
- **Journey latency** (start to final step) is recorded separately; it
  includes all think times and earlier steps.

Step rows in the report are the journey's steps (the text report says
"steps"; the endpoints JSON is named `endpoints` for compatibility), each
with the corrected and service latencies and per-step phases.

## Reporting

Journeys do not fork the report: a run carries the usual latencies, stages,
timeline and generator health (over the per-step figures), and adds journey
fields under `journey` in the JSON summary and in `RunInfo`: the scenario
name, the session counters, and counts by journey-specific failure kind
(`render`, `check`, `extraction`). `gmeter diff` compares a journey run with
another with the usual `-threshold`/`-endpoint-threshold`/`-error-threshold`
gates.

## With `gmeter find`

`gmeter find -scenario FILE -slo p99=50ms` replaces the trial's load profile
with warm-up and measured **session-rate** stages, using the same search and
confirmation rules. The report says `sessions/s`. The error rate now includes
check, extraction and incomplete-journey failures; a trial with too few
completed sessions to judge is a failure, not a pass.

## With `gmeter mcp`

`gmeter mcp -scenario-dir DIR` serves journeys to agents by name: `load_test`
takes `journey: {file: "checkout.yaml", rate: 5, duration: "10s"}`. The
policy then applies:

- The effective request rate is the session rate times the step count and
  must fit `-max-rate` (`-max-rate 1000` with `-max-workers 200` bounds a
  journey of ≤ 200 sessions/s × 5 steps).
- Steps using POST, PUT, PATCH or DELETE are refused unless the operator
  enabled `Policy.AllowWriteScenarios`; the refusal names the setting.
- The scenario is capped: a 1MB file, ≤ 100 steps (the label limit), ≤ 10,000
  CSV rows and ≤ 100 CSV columns read into memory, a 64KB rendered
  path/header/body, and a 4KB extracted value.
- Duration of one session and of a search is capped by `-max-duration`; a
  per-step timeout still applies to every step, so a stalled session cannot
  hold a worker forever.
- Only loopback and allowed targets as usual, and `CheckTarget` applies to
  the base URL. Extracted values never appear in tool output or stored
  reports.

## History

This feature was designed first, in [`docs/design/journeys.md`](design/journeys.md);
the design is now implemented, with the differences called out where the
implementation is simpler (no `gmeter journey` `-agents` support yet: journeys
run on one machine).
