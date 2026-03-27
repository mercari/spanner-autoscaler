# Development

This doc explains how to set up a local environment for development and testing of the spanner-autoscaler and the spanner-autoscale-schedule controllers.

## Tilt (recommended for active development)

[Tilt](https://tilt.dev) automates the full local development loop: it watches your source files, rebuilds the controller, and restarts it automatically on every change. It also manages local emulators for Spanner and Cloud Monitoring so no real GCP credentials are required.

### Prerequisites

- [Tilt](https://docs.tilt.dev/install.html) (`brew install tilt` on macOS)
- [kind](https://kind.sigs.k8s.io/) and [kubectl](https://kubernetes.io/docs/tasks/tools/)
- Docker (for emulators via docker-compose)

> **Tip:** Run `make deps` to install all development tools (`kustomize`, `controller-gen`, `kind`, `golangci-lint`, `crd-ref-docs`, `tilt`, etc.) into `./bin`.

### Usage

```console
# First time (or after make tilt-down): create a kind cluster and start Tilt
$ make tilt-up

# Subsequent runs (cluster already exists)
$ tilt up
```

`make tilt-up` creates a kind cluster if one does not already exist and then runs `tilt up`. Tilt opens a browser UI showing the status of each component. When you edit any Go file under `cmd/`, `api/`, or `internal/`, the controller restarts automatically.

Press `Ctrl-C` to stop Tilt while keeping the cluster and emulators alive (useful for a quick restart).

To tear everything down completely — Tilt resources, emulators, kind cluster, and webhook certs:

```console
$ make tilt-down
```

This runs the following steps in order:
1. `tilt down` — removes all Kubernetes resources Tilt applied
2. `make emulator-down` — stops the docker-compose emulators
3. Deletes the kind cluster
4. Removes `bin/webhook-certs/`

After `make tilt-down`, running `make tilt-up` starts from a completely clean state.

To stream the controller logs while Tilt is running:

```console
$ make logs-controller
```

This runs `tilt logs -f controller` and follows the output until interrupted with `Ctrl-C`.

### What Tilt manages

| Component | Details |
|---|---|
| Spanner emulator | Started via docker-compose (gRPC :9010, admin :9011) |
| Monitoring emulator | Started via docker-compose (gRPC :9090, admin :9091) |
| Emulator instance | `beta-project/beta-instance` created in the Spanner emulator automatically |
| cert-manager | Installed in the kind cluster on first run (skipped if already present) |
| CRDs + webhook config | Applied via `config/deploy-dev` kustomize overlay |
| TLS certificates | Extracted from the cluster and written to `bin/webhook-certs/` |
| Controller | Runs as a local process connected to the emulators; auto-restarts on file change |
| Sample resource | `config/samples/spanner_v1beta1_spannerautoscaler_local.yaml` applied automatically |

### Emulators

Both emulators are managed by `docker-compose.yml` and start automatically with `make tilt-up` (or `make emulator-up`). The controller connects to them via `--spanner-endpoint=localhost:9010` and `--metrics-endpoint=localhost:9090`, which the Tiltfile passes automatically.

To manage the emulators independently:

```console
$ make emulator-up    # start
$ make emulator-down  # stop
```

#### Spanner emulator

The standard [Google Cloud Spanner emulator](https://cloud.google.com/spanner/docs/emulator) runs on:

| Port | Protocol | Purpose |
|---|---|---|
| 9010 | gRPC | Spanner API (used by the controller) |
| 9011 | HTTP | Admin API (instance management) |

Although the Spanner emulator supports the full Spanner API, spanner-autoscaler only uses two RPC methods from the [Instance Admin API](https://cloud.google.com/spanner/docs/reference/rpc/google.spanner.admin.instance.v1):

| RPC | Used in | Purpose |
|---|---|---|
| `InstanceAdmin.GetInstance` | `spanner.Client.GetInstance`, `spanner.Client.UpdateInstance` | Read current processing units and instance state |
| `InstanceAdmin.UpdateInstance` | `spanner.Client.UpdateInstance` | Update `processing_units` (only this field is sent in the field mask) |

Spanner instances must be created via the admin HTTP API before the controller can use them. Tilt does this automatically via the `setup-emulator-instance` resource, but you can also do it manually:

```console
# Create an instance
$ curl -X PUT http://localhost:9011/instances/beta-project/beta-instance \
    -H 'Content-Type: application/json' \
    -d '{"processing_units": 1000}'
```

#### Monitoring emulator

The Monitoring emulator is a **custom implementation** (not the official GCP emulator) that partially simulates the [Cloud Monitoring API](https://cloud.google.com/monitoring/api/ref_v3/rest). It runs on:

| Port | Protocol | Purpose |
|---|---|---|
| 9090 | gRPC | Cloud Monitoring API (used by the controller) |
| 9091 | HTTP | Admin API (configure CPU metrics) |

spanner-autoscaler calls only one RPC method from the [MetricService API](https://cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.timeSeries/list):

| RPC | Used in | Purpose |
|---|---|---|
| `MetricService.ListTimeSeries` | `metrics.Client.GetInstanceMetrics` | Fetch `spanner.googleapis.com/instance/cpu/utilization_by_priority` (priority=`high`) for the target instance |

All other `MetricService` methods (`CreateTimeSeries`, `ListMetricDescriptors`, etc.) are **not implemented** and return `Unimplemented`.

It supports three modes, checked in priority order:

**1. Scenario mode** (priority 1 — default when `SCENARIO_FILE` is set)

Steps through a time-based sequence of CPU values that loops indefinitely. This is the default for `make tilt-up`, which loads `scenarios/default.yaml`. It demonstrates a full scale-up → stable → scale-down cycle without any manual intervention.

Scenario files are written in YAML. Each step uses either a fixed `cpu_utilization` or a `workload` (which computes CPU dynamically based on current PU — see workload mode below):

```yaml
# scenarios/default.yaml
instances:
  - project: beta-project
    instance: beta-instance
    steps:
      - duration: 60s
        workload:
          cpu_utilization: 0.80
          reference_processing_units: 1000
      - duration: 60s
        cpu_utilization: 0.15
```

A scenario can also be set or replaced at runtime via the admin API:

```console
$ curl -X PUT http://localhost:9091/scenario/beta-project/beta-instance \
    -H 'Content-Type: application/json' \
    -d '{"steps": [{"duration": "30s", "cpu_utilization": 0.8}, {"duration": "30s", "cpu_utilization": 0.1}]}'

# Remove the scenario
$ curl -X DELETE http://localhost:9091/scenario/beta-project/beta-instance
```

**2. Workload mode** (priority 2)

Models real Cloud Spanner behaviour: CPU decreases proportionally when processing units increase.

```
cpu = (reference_cpu × reference_pu) / current_pu
```

Requires `SPANNER_EMULATOR_HOST` to be set (already configured in `docker-compose.yml`) so the emulator can query the current PU count from the Spanner emulator.

```console
# Set workload: 80% CPU at 1000 PU baseline
$ curl -X PUT http://localhost:9091/workload/beta-project/beta-instance \
    -H 'Content-Type: application/json' \
    -d '{"cpu_utilization": 0.80, "reference_processing_units": 1000}'

# Remove
$ curl -X DELETE http://localhost:9091/workload/beta-project/beta-instance
```

**3. Static mode** (priority 3)

Returns a fixed CPU utilization value regardless of current processing units. Useful for targeted testing of a specific threshold.

```console
# Set CPU to 75%
$ curl -X PUT http://localhost:9091/metrics/beta-project/beta-instance \
    -H 'Content-Type: application/json' \
    -d '{"cpu_utilization": 0.75}'

# Check current value
$ curl http://localhost:9091/metrics/beta-project/beta-instance

# Remove
$ curl -X DELETE http://localhost:9091/metrics/beta-project/beta-instance
```

If no mode is configured for an instance, the controller logs `no such spanner instance metrics` and skips scaling for that cycle.

### Webhook mode (default)

Webhooks are **enabled by default** because CRD field additions — which also require webhook changes — happen frequently in this project. cert-manager is installed automatically on the first `tilt up` (takes ~1–2 minutes). Subsequent runs skip the install and start in seconds.

### Controller-only mode (no webhooks)

When working on pure scaling logic or metrics without changing any CRD fields:

```console
$ tilt up -- --enable_webhooks=false
```

Or create a `tilt_config.json` file (gitignored):

```json
{"enable_webhooks": false}
```

This skips cert-manager, webhook config, and TLS certificate setup entirely.

---

## Quick start (no webhooks)

For most controller development (scaling logic, metrics, etc.), webhooks are not needed. `make run` disables them automatically:

```console
$ make run
```

This is the fastest way to iterate. No kind cluster or TLS certificates required — just a kubeconfig pointing to any cluster with the CRDs installed.

To install CRDs into a cluster:

```console
$ make kind-cluster-reset  # create a kind cluster
$ make install             # install CRDs
$ kubectl apply -k config/samples
```

## Local webhook development (without Tilt)

If you prefer not to use Tilt, you can run the full webhook development loop manually:

1. Create (or reset) a kind cluster and deploy the dev overlay:
   ```console
   $ make kind-cluster-reset
   $ make deploy-dev
   ```
   This deploys all CRDs and resources, scales the in-cluster controller to 0, and routes webhook traffic to `host.docker.internal:9443` (your local machine).

1. Run the controller locally with webhook forwarding:
   ```console
   $ make run-dev
   ```
   `run-dev` automatically extracts the webhook TLS certificate from the cluster into `bin/webhook-certs/` and starts the controller with `--cert-dir` pointing to that directory.

1. Apply sample resources:
   ```console
   $ kubectl apply -k config/samples
   ```

1. To test further changes, stop the controller (`Ctrl-C`) and run `make run-dev` again. The certs are re-extracted on each invocation (in case they were rotated).

## Full cluster deployment

For end-to-end testing with the controller running inside the cluster:

```console
$ make kind-cluster-reset
$ export IMG=mercari/spanner-autoscaler:local
$ make docker-build kind-load-docker-image
$ make install deploy
$ kubectl apply -k config/samples
```

---

## Testing

### Unit and webhook tests

```console
$ make test
```

This runs all tests in `./...`, including:

- **Controller unit tests** (`internal/controller/`) — reconciler logic using `envtest` (a real API server).
- **Webhook validation tests** (`api/v1beta1/`) — integration tests that exercise the admission webhook via `envtest`'s `WebhookInstallOptions`. These verify that invalid `SpannerAutoscaler` and `SpannerAutoscaleSchedule` resources are rejected, and that immutable fields cannot be changed after creation.

`make test` automatically downloads the required `envtest` binaries if they are not present.

### Integration tests

Integration tests run against the live Spanner and Cloud Monitoring emulators. `make test-integration` starts the emulators automatically if they are not already running:

```console
$ make test-integration
```

To manage the emulators manually before running:

```console
$ make emulator-up
$ go test -tags integration ./test/integration/... -v -count=1
$ make emulator-down
```

---

## Modifying CRD fields

When you add or change fields in a CRD type file (`api/*/*_types.go`):

1. **Update the type** in `api/v1beta1/*_types.go` (or `api/v1alpha1/`).
2. **Add or update webhook validation** in `api/v1beta1/*_webhook.go`.
3. **Add webhook tests** in `api/v1beta1/*_webhook_test.go` to cover the new validation rules.
4. **Regenerate manifests and deepcopy code**:
   ```console
   $ make manifests generate
   ```
5. **Run all tests** to verify nothing is broken:
   ```console
   $ make test
   ```

---

## Before sending a PR

- Run `make manifests generate` if you made any changes to CRD definitions (`api/*/*_types.go`).
- Run `make docs` if you changed anything that affects the CRD reference documentation.
- Run `make test` and ensure all tests pass.
- Run `make lint` and fix any reported issues.
