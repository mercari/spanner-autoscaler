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

3. **Search** for a better configuration. Candidate values replace the
   manifest's value for that parameter; unspecified parameters keep the
   manifest's value. `-auto` generates step-size and interval candidates
   within the recommended PU-change limits.

   ```console
   $ ./bin/simulator recommend -metrics metrics.csv -config current.yaml \
       -min-pu 16000,18000,20000 -scaledown-step-size "10%,30%" \
       -max-exceeded-minutes 500 -html report.html
   ```

   `compare` replays several complete manifests side by side instead of
   searching.

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

Rejected candidates are listed with the specific constraint they broke.

## Reading the results

- The text output and the HTML report both start with the conclusion: each
  parameter as `current → recommended`, with the cost and risk deltas of
  adopting the top candidate.
- The min PU assessment answers the two directions separately: whether the
  minimum can go lower (time pinned at the minimum and the workload's p95
  requirement while pinned) and whether raising it would absorb overshoot
  (the share of above-target minutes observed at the minimum).
- `low confidence` counts the minutes whose recorded CPU exceeds
  `-low-confidence-cpu` (default 50%). The counterfactual CPU model assumes
  `cpu = workload / PU`; when the recorded CPU was that high, the workload
  itself may have been throttled, so treat simulated results in those
  periods conservatively.
- Before adopting a candidate, validate it against a period that was not
  used for the search (for example, the previous month) and check the
  storage floor: the minimum processing units must also cover the
  database's storage requirement.
