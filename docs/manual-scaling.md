# SpannerManualScaling

`SpannerManualScaling` is a one-shot operation resource that pins a
`SpannerAutoscaler` to an explicit processing-units target for incident
response or planned ramps. While an override is active, it takes
precedence over CPU- and schedule-driven autoscaling on the parent
autoscaler.

The operational surface is intentionally just **create** and **delete**:
spec is immutable, so "modify" is expressed as "create a new resource",
and the newest-`creationTimestamp` rule atomically swaps the active
override without an autoscaling gap.

## Highlights

- **Spec is immutable** after creation. Changes are expressed by creating
  a new `SpannerManualScaling` — the newest `creationTimestamp` becomes
  Active and atomically supersedes the older resource. To force
  expiration, `kubectl delete` the resource.
- **Pacing is inferred** from the presence of the step-size field for
  the active direction. `scaleupStepSize` set → stepped scale-up; unset
  → single-jump. The interval falls back to the controller's existing
  `--scale-up-interval` / `--scale-down-interval` flag default when the
  spec field is omitted. Interval-only configuration is treated as
  single-jump and surfaces an admission warning.
- **`expiresAt`** accepts any RFC 3339 timezone designator (`Z`,
  `+09:00`, etc.). Storage is normalized to UTC per Kubernetes
  convention; the absolute instant is preserved.
- **`--reject-manual-scaledown`** cluster-wide policy flag: when
  enabled, the webhook and the reconciler refuse any
  `SpannerManualScaling` whose target PU is below the parent's current
  PU. Lets cluster operators make `SpannerManualScaling` structurally
  scale-up-only and grant create/delete permission to a wider operator
  audience at lower risk.
- **`--manual-scaling-history-per-namespace=N`** opt-in history GC:
  keeps the N most-recent finished (Expired/Superseded/Invalid)
  resources per namespace and deletes the older ones.
  Active/Progressing/Pending are never touched.

## Lifecycle phases

The `status.phase` field surfaces where the override is in its
lifecycle:

| Phase | Meaning | Terminal? |
|---|---|---|
| `Pending` | Created but not yet picked up by the controller. Transient — typically resolved on the first reconcile. | no |
| `Progressing` | A step size is set and `currentProcessingUnits` has not yet reached `spec.processingUnits`. The controller is taking step-bounded, interval-gated updates toward the target. | no |
| `Active` | `currentProcessingUnits` equals `spec.processingUnits`; the override is holding the target on the parent autoscaler. A single-jump override (step size unset) skips `Progressing` and goes `Pending` → `Active`. | no |
| `Expired` | `expiresAt` has elapsed. With a step size set, this can occur mid-ramp; in that case the target may not have been reached. | yes |
| `Superseded` | A newer `SpannerManualScaling` for the same `targetResource` was created and is now active. | yes |
| `Invalid` | The `targetResource` does not exist (in the same namespace) or the override otherwise cannot be applied. To retry after fixing the cause, create a new `SpannerManualScaling` — the controller will not transition this resource back to a non-terminal phase. | yes |

Terminal-phase resources are retained for operator inspection until the
history-limit GC reclaims them (see below).

## Usage

### Create — single-jump (incident response)

Apply the target in one reconcile. Useful when you need PU raised right
now and the cost of a sudden change is acceptable.

```bash
kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: incident-2026-06-01
  namespace: production
spec:
  targetResource: spannerautoscaler-sample
  processingUnits: 7000
  expiresAt: "2026-06-01T22:00:00+09:00"   # JST input; stored as UTC
EOM
```

### Create — stepped ramp (planned scale-up for an event)

Walk toward the target in step-size increments. Useful for planned ramps
where you want to avoid sudden Spanner split redistribution.
`scaleupInterval` defaults to the controller's `--scale-up-interval`
flag when omitted.

```bash
kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: event-ramp-2026-06-01
  namespace: production
spec:
  targetResource: spannerautoscaler-sample
  processingUnits: 9000
  scaleupStepSize: 1000   # at most +1000 PU per step
  scaleupInterval: 5m     # wait at least 5 minutes between steps
  expiresAt: "2026-06-01T20:00:00+09:00"
EOM
```

A common shortcut for one-off overrides is `generateName:` so the
operator doesn't have to think of a unique `name:` each time:

```yaml
metadata:
  generateName: incident-
  namespace: production
```

### Inspect

The active override on each autoscaler is visible on the parent's
printer columns:

```bash
$ kubectl get spannerautoscaler -n production
NAME                       PROJECT ID  ...  CURRENT PUS  DESIRED PUS  MANUAL PU  MANUAL RAMP
spannerautoscaler-sample   my-proj     ...  4000         5000         9000       true
```

Listing the manual scalings shows the lifecycle phase of each:

```bash
$ kubectl get spannermanualscaling -n production
NAME                            TARGET                     TARGET PU  CURRENT PU  EXPIRES AT             PHASE         AGE
event-ramp-2026-06-01           spannerautoscaler-sample   9000       5000        2026-06-01T11:00:00Z   Progressing   12m
```

Events on the parent `SpannerAutoscaler` trace what the controller did:

```bash
$ kubectl describe spannerautoscaler spannerautoscaler-sample -n production | tail
Events:
  Type    Reason                    Age   From                          Message
  ----    ------                    ----  ----                          -------
  Normal  ManualScalingApplied      12m   spannerautoscaler-controller  manual scaling applied: pinned PU to 4000 (source=event-ramp-2026-06-01)
  Normal  ManualScalingProgressing  7m    spannerautoscaler-controller  manual scaling step: 4000 -> 5000 (target=9000, source=event-ramp-2026-06-01)
```

### Modify — create a new resource (the spec is immutable)

To raise the target, change the ramp pace, or extend `expiresAt`, create
a new `SpannerManualScaling`. The older one is automatically marked
`Superseded` and the new one becomes `Active` in the same reconcile —
the Spanner instance does not return to autoscale-driven control in
between.

```bash
# Override the existing ramp with a faster target (still stepped):
kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: event-ramp-2026-06-01-v2     # different name; same target
  namespace: production
spec:
  targetResource: spannerautoscaler-sample
  processingUnits: 12000
  scaleupStepSize: 2000               # bigger step
  scaleupInterval: 2m                 # shorter cooldown
  expiresAt: "2026-06-01T20:00:00+09:00"
EOM

# Confirm: older is Superseded, newer is Active.
$ kubectl get spannermanualscaling -n production
NAME                            TARGET                     TARGET PU  CURRENT PU  EXPIRES AT             PHASE        AGE
event-ramp-2026-06-01           spannerautoscaler-sample   9000       5000        2026-06-01T11:00:00Z   Superseded   15m
event-ramp-2026-06-01-v2        spannerautoscaler-sample   12000      5000        2026-06-01T11:00:00Z   Progressing  4s
```

Attempting to mutate an existing resource is rejected by the webhook:

```bash
$ kubectl patch spannermanualscaling event-ramp-2026-06-01 -n production --type=merge \
    -p '{"spec":{"processingUnits":12000}}'
Error from server (Invalid): admission webhook "vspannermanualscaling.kb.io" denied the request:
SpannerManualScaling.spanner.mercari.com "event-ramp-2026-06-01" is invalid: spec: Forbidden:
spec is immutable; create a new SpannerManualScaling instead. The newest creationTimestamp becomes
Active and supersedes the older resource atomically (no autoscaling gap). To force expiration,
kubectl delete this resource.
```

### Delete — force-deactivate the override

`kubectl delete` is the "force-expire" mechanism. The parent autoscaler
resumes CPU- and schedule-driven control on the next reconcile.

```bash
kubectl delete spannermanualscaling event-ramp-2026-06-01-v2 -n production
```

If you want every active override on a target gone (matched by a label
the operator set):

```bash
kubectl delete spannermanualscaling \
  -n production \
  -l app.kubernetes.io/instance=event-ramp-2026-06-01
```

## Cluster operator features

### `--reject-manual-scaledown=true`

When the controller is started with `--reject-manual-scaledown=true`,
attempts to set a target PU below the current PU are rejected at
admission time:

```bash
$ kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: try-shrink
  namespace: production
spec:
  targetResource: spannerautoscaler-sample   # currently 6000 PU
  processingUnits: 4000
EOM
Error from server (Forbidden): admission webhook "vspannermanualscaling.kb.io" denied the request:
SpannerManualScaling.spanner.mercari.com "try-shrink" is invalid: spec.processingUnits: Forbidden:
spec.processingUnits=4000 would reduce target SpannerAutoscaler "spannerautoscaler-sample" from
currentProcessingUnits=6000. Cluster policy --reject-manual-scaledown=true forbids scaledown via
SpannerManualScaling.
```

The check is enforced in two places (defense in depth): the validating
webhook (admission time) and the reconciler (which marks
`Invalid` + emits a `ManualScalingRejected` event if a forbidden
override somehow slips through admission — e.g. when the webhook was
bypassed). Under this policy, the `SpannerManualScaling` CRUD surface
is structurally scale-up-only, so create/delete RBAC can be granted to
a wider operator audience without risk of triggering a service-impacting
PU reduction.

### `--manual-scaling-history-per-namespace=N`

Default `0` (disabled). When set to a positive integer, the controller
keeps the N most-recent finished (`Expired` / `Superseded` / `Invalid`)
`SpannerManualScaling` resources per namespace and deletes the older
ones by `status.finishedAt`. Active / Progressing / Pending resources
are never touched by this GC.

This is opt-in to keep the default behavior bit-for-bit identical to a
deployment without the flag, and to protect existing users that depend
on the audit trail being kept indefinitely.

## RBAC

The repo ships two ClusterRole templates as starting points for
namespace-scoped RoleBindings:

- [`config/rbac/spannermanualscaling_editor_role.yaml`](../config/rbac/spannermanualscaling_editor_role.yaml)
  — grants `create`, `delete`, `get`, `list`, `watch`. Spec immutability
  is enforced by the webhook, so granting the editor role does not
  expose a "modify-in-place" surface.
- [`config/rbac/spannermanualscaling_viewer_role.yaml`](../config/rbac/spannermanualscaling_viewer_role.yaml)
  — grants `get`, `list`, `watch`.

When the cluster runs with `--reject-manual-scaledown=true`, the editor
role is effectively scale-up-only: any `SpannerManualScaling` whose
target PU is below the parent's current PU is rejected at admission, so
holders of the role cannot trigger a PU reduction.

## CRD field reference

See the generated [`docs/crd-reference.md`](./crd-reference.md) for the
exhaustive field-by-field reference (types, defaults, validation
rules) generated by `crd-ref-docs` from the Go type annotations.

## Local end-to-end testing

This walkthrough mirrors a real verification run against a kind cluster
with the Spanner / Cloud Monitoring emulators via the existing Tilt
setup (see [development.md](./development.md)).

### Setup

```bash
# 1. Install local dev tools (tilt, kind, kustomize, etc. into ./bin)
$ make deps

# 2. Spin up the full stack: kind cluster, emulators, dev TLS, controller
$ make tilt-up
```

After `tilt up` converges, the sample `SpannerAutoscaler`
(`spannerautoscaler-sample-beta`) is applied and the controller process
is connected to the local emulators on `localhost:9010` /
`localhost:9090`.

Set a Cloud Monitoring scenario so the syncer has data to refresh
`status.currentProcessingUnits` with:

```bash
$ curl -s -X PUT http://localhost:9091/scenario/beta-project/beta-instance \
    -H 'Content-Type: application/json' \
    -d '{"steps":[{"duration":"3600s","high_priority":{"cpu_utilization":0.20},"total":{"cpu_utilization":0.15}}]}'
```

Without a scenario, the monitoring emulator returns "no such spanner
instance metrics" and the syncer leaves `status.currentProcessingUnits`
stale. The override itself is still applied to the Spanner instance —
only the status read-back is blocked.

### Step 1: Create a single-jump override

```bash
$ kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: incident-2026-06-01
  namespace: spanner-autoscaler
spec:
  targetResource: spannerautoscaler-sample-beta
  processingUnits: 7000
EOM
spannermanualscaling.spanner.mercari.com/incident-2026-06-01 created
```

After the controller reconciles (a few seconds), the override reaches
the Active phase and the parent autoscaler's printer columns reflect
the override:

```bash
$ kubectl get spannermanualscaling -n spanner-autoscaler
NAME                  TARGET                          TARGET PU   CURRENT PU   EXPIRES AT   PHASE    AGE
incident-2026-06-01   spannerautoscaler-sample-beta   7000        7000                      Active   5m32s

$ kubectl get spannerautoscaler -n spanner-autoscaler spannerautoscaler-sample-beta
NAME                            ...  CURRENT PUS   DESIRED PUS   MANUAL PU   MANUAL RAMP   AGE
spannerautoscaler-sample-beta   ...  7000          7000          7000        false         47d

$ kubectl get events -n spanner-autoscaler --field-selector involvedObject.name=spannerautoscaler-sample-beta | grep -i manual
6s   Normal   ManualScalingApplied   spannerautoscaler/spannerautoscaler-sample-beta   manual scaling applied: pinned PU to 7000 (source=incident-2026-06-01)
```

### Step 2: Attempt to patch the spec (must be rejected)

```bash
$ kubectl patch spannermanualscaling incident-2026-06-01 -n spanner-autoscaler --type=merge \
    -p '{"spec":{"processingUnits":4000}}'
The SpannerManualScaling "incident-2026-06-01" is invalid: spec: Forbidden: spec is immutable;
create a new SpannerManualScaling instead. The newest creationTimestamp becomes Active and
supersedes the older resource atomically (no autoscaling gap). To force expiration, kubectl
delete this resource.
```

The webhook blocks any spec mutation. `expiresAt`, `processingUnits`,
ramp fields, and `targetResource` are all rejected with the same
message — there is no field-level "this is editable" carve-out.

### Step 3: Create a second resource on the same target (newest-wins, with warning)

```bash
$ kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: event-ramp-2026-06-01
  namespace: spanner-autoscaler
spec:
  targetResource: spannerautoscaler-sample-beta
  processingUnits: 4000
  scaledownStepSize: 1000
  scaledownInterval: 15s
EOM
Warning: A SpannerManualScaling for targetResource="spannerautoscaler-sample-beta" already exists
(name="incident-2026-06-01", phase=Active). Creating this resource will supersede it (the older
resource transitions to phase=Superseded). If unintended, delete this new resource and inspect
the existing one first.
spannermanualscaling.spanner.mercari.com/event-ramp-2026-06-01 created
```

A few seconds later, the previous override has transitioned to
`Superseded` and the new one is `Progressing` toward its lower target:

```bash
$ kubectl get spannermanualscaling -n spanner-autoscaler
NAME                    TARGET                          TARGET PU   CURRENT PU   EXPIRES AT   PHASE         AGE
event-ramp-2026-06-01   spannerautoscaler-sample-beta   4000        7000                      Progressing   5s
incident-2026-06-01     spannerautoscaler-sample-beta   7000        7000                      Superseded    6m7s

$ kubectl get spannerautoscaler -n spanner-autoscaler spannerautoscaler-sample-beta
NAME                            ...  CURRENT PUS   DESIRED PUS   MANUAL PU   MANUAL RAMP   AGE
spannerautoscaler-sample-beta   ...  7000          6000          4000        true          47d
```

`MANUAL RAMP` flipped to `true` because the new override has
`scaledownStepSize` set; `MANUAL PU` is the final target (4000), not
the in-flight value.

### Step 4: Stepped ramp progression

Watching the same override over three minutes — each 15-second interval
drops `currentProcessingUnits` by the configured 1000:

```text
T+30s:   event-ramp-2026-06-01   4000   6000   Progressing
T+60s:   event-ramp-2026-06-01   4000   6000   Progressing
T+90s:   event-ramp-2026-06-01   4000   5000   Progressing
T+120s:  event-ramp-2026-06-01   4000   5000   Progressing
T+150s:  event-ramp-2026-06-01   4000   4000   Active
T+180s:  event-ramp-2026-06-01   4000   4000   Active
```

The `Active` transition stamps `status.reachedAt`:

```yaml
status:
  appliedAt: "2026-06-01T02:15:50Z"
  currentProcessingUnits: 4000
  phase: Active
  reachedAt: "2026-06-01T02:18:16Z"
```

### Step 5: Force-deactivate via kubectl delete

```bash
$ kubectl delete spannermanualscaling event-ramp-2026-06-01 -n spanner-autoscaler
spannermanualscaling.spanner.mercari.com "event-ramp-2026-06-01" deleted from spanner-autoscaler namespace

$ kubectl get spannermanualscaling -n spanner-autoscaler
NAME                  TARGET                          TARGET PU   CURRENT PU   EXPIRES AT   PHASE        AGE
incident-2026-06-01   spannerautoscaler-sample-beta   7000        7000                      Superseded   9m54s

$ kubectl get spannerautoscaler -n spanner-autoscaler spannerautoscaler-sample-beta \
    -o custom-columns=NAME:.metadata.name,CURRENT_PUS:.status.currentProcessingUnits,MANUAL_PU:.status.activeManualScaling.processingUnits
NAME                            CURRENT_PUS   MANUAL_PU
spannerautoscaler-sample-beta   4000          <none>
```

`status.activeManualScaling` is cleared (`<none>` in the column) and
the autoscaler is back under CPU-driven control. The remaining
`Superseded` resource is kept as audit history (deleted on demand or by
the `--manual-scaling-history-per-namespace` GC once that flag is
enabled).

### Step 6: Event trail on the parent

A consolidated view of what the controller did over the test run:

```bash
$ kubectl get events -n spanner-autoscaler \
    --field-selector involvedObject.name=spannerautoscaler-sample-beta \
    --sort-by=.lastTimestamp | grep -iE "manualscaling"
LAST SEEN   TYPE     REASON                     OBJECT                                            MESSAGE
4m43s       Normal   ManualScalingApplied       spannerautoscaler/spannerautoscaler-sample-beta   manual scaling applied: pinned PU to 7000 (source=incident-2026-06-01)
3m38s       Normal   ManualScalingProgressing   spannerautoscaler/spannerautoscaler-sample-beta   manual scaling step: 7000 -> 6000 (target=4000, source=event-ramp-2026-06-01)
2m38s       Normal   ManualScalingProgressing   spannerautoscaler/spannerautoscaler-sample-beta   manual scaling step: 6000 -> 5000 (target=4000, source=event-ramp-2026-06-01)
98s         Normal   ManualScalingApplied       spannerautoscaler/spannerautoscaler-sample-beta   manual scaling applied: pinned PU to 4000 (source=event-ramp-2026-06-01)
```

### Step 7: `--reject-manual-scaledown=true` cluster policy

Restart the controller with the policy flag set:

```bash
# Edit Tiltfile temporarily (or pass via deployment args in a real cluster):
EMULATOR_FLAGS = '--spanner-endpoint=localhost:9010 --metrics-endpoint=localhost:9090 --leader-elect=false --reject-manual-scaledown=true'

# Then trigger a controller restart so the new flag takes effect:
$ ./bin/tilt trigger controller
```

The controller log confirms the flag is active:

```text
DEBUG  flags  {"rejectManualScaledown": true, "manualScalingHistoryPerNamespace": 0, ...}
```

With `currentProcessingUnits=6000`, the three attempts below cover the
boundary cases the webhook + reconciler defense-in-depth check
distinguishes:

```bash
# A. target PU LOWER than current → expect Forbidden by webhook
$ kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: a-shrink-rejected
  namespace: spanner-autoscaler
spec:
  targetResource: spannerautoscaler-sample-beta
  processingUnits: 1000
EOM
The SpannerManualScaling "a-shrink-rejected" is invalid: spec.processingUnits: Forbidden:
spec.processingUnits=1000 would reduce target SpannerAutoscaler "spannerautoscaler-sample-beta"
from currentProcessingUnits=6000. Cluster policy --reject-manual-scaledown=true forbids
scaledown via SpannerManualScaling.
```

```bash
# B. target PU EQUAL to current → expect accepted (no reduction, so not a scaledown)
$ kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: b-equal-accepted
  namespace: spanner-autoscaler
spec:
  targetResource: spannerautoscaler-sample-beta
  processingUnits: 6000
EOM
spannermanualscaling.spanner.mercari.com/b-equal-accepted created
```

```bash
# C. target PU HIGHER than current → expect accepted (scale-up is always allowed)
$ kubectl apply -f - <<EOM
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerManualScaling
metadata:
  name: c-scaleup-accepted
  namespace: spanner-autoscaler
spec:
  targetResource: spannerautoscaler-sample-beta
  processingUnits: 8000
EOM
Warning: A SpannerManualScaling for targetResource="spannerautoscaler-sample-beta" already exists
(name="b-equal-accepted", phase=Active). Creating this resource will supersede it ...
spannermanualscaling.spanner.mercari.com/c-scaleup-accepted created
```

Final state confirms only scale-up paths landed; the reject case never
created a resource:

```bash
$ kubectl get spannermanualscaling -n spanner-autoscaler
NAME                 TARGET                          TARGET PU   CURRENT PU   EXPIRES AT   PHASE         AGE
b-equal-accepted     spannerautoscaler-sample-beta   6000        6000                      Superseded    1s
c-scaleup-accepted   spannerautoscaler-sample-beta   8000        6000                      Progressing   1s
```
