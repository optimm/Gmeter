# Gmeter in GitHub Actions

Back to the [README](../README.md).

The repository root is a composite action, so a published tag can be used as `uses: optimm/Gmeter@vX.Y.Z`. It runs
Gmeter, puts the text report in the job summary (`$GITHUB_STEP_SUMMARY`), uploads the JSON report as an artifact, and
fails the step when a gate is missed. No new CLI flags were needed: `gmeter find` already exits 1 when no rate meets
the target, and `gmeter diff -threshold` already exits 1 on a p99 regression.

## Choose a mode

| Mode | Set | Command the action runs | Step fails when |
|---|---|---|---|
| Report | only `url` | `gmeter <args> -json <output> <url>` | the run cannot complete |
| SLO gate | `slo` | `gmeter find -slo <slo> <find-args> -json <output> <url>` | no rate meets the target, the generator could not keep time, or the search was interrupted |
| Diff gate | `baseline` | report mode, then `gmeter diff <thresholds> <baseline> <output>` | p99, errors or a replay endpoint regressed beyond the thresholds given |

`slo` and `baseline` are mutually exclusive. A plain `gmeter` run reports warnings but exits 0; use `slo` when a
latency target should fail the build. `gmeter diff` only gates on the thresholds you pass: no threshold means it
prints the comparison and still exits 0.

## Inputs

| Input | Default | Meaning |
|---|---|---|
| `url` | required | Target URL, as it would follow the flags of `gmeter` or `gmeter find`. |
| `args` | empty | Extra flags for report or diff mode, written before the URL, e.g. `-rate 200 -duration 30s -H 'X-Env: staging'`. Do not include the URL or `-json`; the action adds `-json` last. |
| `slo` | empty | A target such as `p99=50ms` or `p99.9=200ms`. Switches to `gmeter find`. |
| `find-args` | empty | Extra flags for `gmeter find`, e.g. `-start 100 -max 2000 -trial 5s -confirm 1`. The action adds `-slo` and `-json`. |
| `baseline` | empty | Path to an earlier JSON summary. Switches on the `gmeter diff` gate. |
| `threshold` | empty | Percent by which corrected p99 may rise before `gmeter diff` fails, e.g. `10`. |
| `error-threshold` | empty | Percentage points by which the error rate may rise before it fails. |
| `endpoint-threshold` | empty | Percent by which a replay endpoint's p99 may rise before it fails; needs replay runs with an endpoint breakdown. |
| `output` | `gmeter-report.json` | Path of the JSON summary to write and upload. |
| `artifact-name` | `gmeter-report` | Name of the uploaded artifact. Give each action call in one job a different name. |

`args` and `find-args` are evaluated by bash, so quote values with spaces as you would in a shell. They are workflow
code, not data from an untrusted source.

### Output

The action exposes `json-path`, the absolute path of the JSON summary. The summary and the artifact both contain the
same path, so a later step can use it without guessing where the file landed.

## Examples

Fail a build when the service no longer holds `p99=50ms`:

```yaml
jobs:
  slo:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: optimm/Gmeter@vX.Y.Z # the tag that contains action.yml
        with:
          url: https://staging.example.com/api
          slo: p99=50ms
          find-args: -start 100 -max 2000 -trial 5s -warmup 1s -confirm 1
```

Compare a candidate against a baseline saved earlier in the same job. Keep the load profile identical: `gmeter diff`
prints a note when the runs were not like-for-like, but a threshold still compares their p99s.

```yaml
jobs:
  diff:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: optimm/Gmeter@vX.Y.Z
        id: before
        with:
          url: https://staging.example.com/api
          args: -rate 200 -duration 30s -workers 50
          output: baseline.json
          artifact-name: gmeter-baseline
      - uses: optimm/Gmeter@vX.Y.Z
        with:
          url: https://staging.example.com/api
          args: -rate 200 -duration 30s -workers 50
          baseline: baseline.json
          threshold: 10
          error-threshold: 0.5
          output: candidate.json
          artifact-name: gmeter-candidate
```

The checked-in [load-test workflow](../.github/workflows/load-test.yml) runs both examples with `uses: ./`, which is
handy while developing the action.

## How Gmeter is installed

For `uses: optimm/Gmeter@vX.Y.Z`, the action reads the pinned ref and downloads the release archive matching the
runner's OS and architecture: `gmeter_<version>_<os>_<arch>.tar.gz`, or `.zip` on Windows, as built by GoReleaser.
It puts the binary on `PATH` for the later steps. On Windows it uses `gmeter.exe`.

When there is no released archive for that ref — for a local `uses: ./`, a branch, a SHA, or a tag whose release is
not published yet — the action falls back to `actions/setup-go` with the action's own `go.mod` and builds
`./cmd/gmeter` from the checked-out action source. That makes a local action work without a prior release and keeps a
tagged action reproducible.

The report is written even when a gate fails: the summary step and the artifact upload run with `always()`. If the run
failed before a JSON file existed, the upload warns instead of hiding the original failure with a second one.

## Reading the summary

The job summary shows the mode, the JSON path, the artifact name, and the run's text report in a collapsible block.
In diff mode a second block contains the comparison, including the `REGRESSION` or `ok` line from `gmeter diff`.
[Reading the report](reading-the-report.md#reading-the-generator-line) explains the generator line: if it says
`generator.limited`, an SLO gate will not pass the run, because the numbers may describe the generator rather than the
server. [Comparing runs](comparing-runs.md) explains what `gmeter diff` checks and how its thresholds are measured.
