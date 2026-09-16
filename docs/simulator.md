# Configuration simulator

`cmd/simulator` evaluates `SpannerAutoscaler` configuration changes (minimum
processing units, step sizes, scale-down windows, CPU targets, schedules)
before they reach production.

It is a **backtest over recorded metrics** (trace-driven simulation), not a
forecast: the workload input is actual history fetched from Cloud Monitoring,
and the simulated part is the counterfactual — what the autoscaler would have
done against that workload under a different configuration. Decisions are
computed by `internal/scaling`, the same package the controller uses, so
replays match production behavior including step sizes, cooldown intervals,
`scaledownAllowedTimes` windows, and `SpannerAutoscaleSchedule` activation.

## Build

```console
$ make build-simulator
$ ./bin/simulator help
```

## Workflow

1. **Fetch** the recorded metrics (requires Application Default Credentials
   with `roles/monitoring.viewer` on the project). Use a period that covers
   the workload's monthly batch cycle — four weeks or more is recommended;
   a period without the heavy days underestimates risk.

   ```console
   $ ./bin/simulator fetch -project <project> -instance <instance> \
       -start 2026-08-11T00:00:00Z -out metrics.csv
   ```

   Fetching directly from Cloud Monitoring matters: per-minute aggregates
   exported through other pipelines can smooth out the short CPU spikes the
   controller actually reacts to. If the simulation starts while a schedule
   is active, that schedule is not reproduced — trim the CSV so it starts at
   a time when no schedule is running.

2. **Replay** the current manifest to check reproducibility. The manifest
   file may contain the `SpannerAutoscaler` and its
   `SpannerAutoscaleSchedule`s as multiple YAML documents; the production
   defaulting webhook is applied on load.

   ```console
   $ ./bin/simulator simulate -metrics metrics.csv -config current.yaml \
       -html report.html
   ```

3. **Search** for a better configuration. With `-auto`, the candidates are
   generated from the data and the guidelines, so no parameter values need
   to be supplied:

   ```console
   $ ./bin/simulator recommend -metrics metrics.csv -config current.yaml \
       -auto -max-exceeded-minutes 500 -html report.html
   ```

   `-auto` generates the candidates as follows; each generated dimension
   also keeps the configuration's current value in the running:

   - `-min-pu`: percentiles (p50–p99) of the PU the recorded workload
     actually required to stay on its CPU targets.
   - `-scaledown-step-size`: 5%, 10%, 15%, and 20%. Larger steps shed
     capacity too fast to recommend for an instance that serves steady
     traffic; list them explicitly to search them.
   - `-scaledown-interval` / `-scaleup-interval`: the PU-change guideline
     gaps (10m and 30m).
   - `scaleupStepSize` is not searched: capping the upward step saves almost
     nothing while delaying spike response.

   Explicit candidate lists (for example `-min-pu 16000,18000,20000`)
   replace the manifest's value for that parameter and take precedence over
   `-auto`; unspecified parameters keep the manifest's value. `compare`
   replays several complete manifests side by side instead of searching.

   A candidate is recommended only when it satisfies every constraint and
   costs less than the current configuration's own replay; otherwise the
   conclusion says to keep the current configuration.

   The recommendation is staged: `-max-changes` (default 1) restricts it to
   candidates changing that many parameters at once — adopt one change,
   observe, re-run against fresh metrics for the next one. Among candidates
   whose savings are within `-savings-tolerance` percentage points of the
   best (default 2.0), the least risky wins: the lowest scale-down rate
   first (step size ÷ interval, the PU shed per minute — resize churn is a
   cost the simulation cannot measure), then the measured risk counters. A
   candidate that saves more than the tolerance beyond the recommendation
   appears as a "further option" to try after the recommended change has
   proven out.

## Constraints and guidelines

`recommend` ranks the candidates that satisfy every constraint by simulated
PU-hours, cheapest first:

- `-max-exceeded-minutes`: total minutes the simulated CPU may spend above
  its target.
- `-instance-config regional|multi-region|none` (default `regional`): applies
  the Google-recommended high-priority CPU maximum (65% for regional, 45% per
  region for multi-region instance configurations) to both the configured
  target and the simulated p99.
- `-pu-change-guideline base|strict|none` (default `base`): applies the
  recommended compute-capacity change limits (at most 2x/half per operation,
  at least 10 minutes between operations). `base` requires candidates to be
  no worse than the current configuration's own replay, so violations caused
  by fixed schedules do not reject the whole grid.
- `-max-p99-cpu`: an additional cap on the p99 of every simulated CPU metric
  (0 disables it).

Rejected candidates are listed with the specific constraint they broke, and
candidate tables show one row per distinct simulated outcome — parameter
combinations that behave identically on the recording fold into it as
"(+N equivalent)".

## Reading the results

- The text output and the HTML report both start with the conclusion: each
  parameter as `current → recommended` (unchanged parameters marked "keep",
  intervals the spec leaves unset shown as "controller default"), with the
  cost and risk deltas of adopting the recommended candidate and, when one
  exists, the further option.
- The `recommend` HTML report embeds the recommended candidate's simulated
  PU and CPU timelines next to the recorded ones, so the behavior under the
  recommended configuration — for example, how close the CPU would have come
  to its target with a lower minimum — can be inspected over time before
  adopting it.
- The min PU assessment answers the two directions separately: whether the
  minimum can go lower (time pinned at the minimum and the workload's p95
  requirement while pinned) and whether raising it
  would reduce the time above target (the share of above-target minutes
  observed while the instance sits at the minimum).
- `low confidence` counts the minutes whose recorded CPU exceeds
  `-low-confidence-cpu` (default 50%). The counterfactual CPU model assumes
  `cpu = workload / PU`; when the recorded CPU was that high, the workload
  itself may have been throttled, so treat simulated results in those
  periods conservatively.
- Before adopting a candidate, validate it against a period that was not
  used for the search (for example, the previous month) and check the
  storage floor: the minimum processing units must also cover the
  database's storage requirement.
- CEL scaling rules (`spec.scaleConfig.scalingRules`) and the
  `scaleupCondition` / `scaledownCondition` gates replay with production
  semantics: the metric-window aggregates (`metricWindows`) are recomputed
  from the simulated CPU series at every tick, including the warm-up period
  during which a window has too little data and expressions are skipped
  fail-safe. `summary.celErrors` counts expression evaluations that failed
  outside the warm-up — non-zero means the same manifest would also error in
  production (rules skipped, gates falling back to their fail-safe
  direction), so fix the expressions before applying.
