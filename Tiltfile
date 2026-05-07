# -*- mode: Python -*-

# ── Configuration ──────────────────────────────────────────────────────────────
# Webhooks are enabled by default because CRD field additions (which require
# webhook changes) are frequent in this project.
#
# To disable webhooks (for pure controller logic development):
#   tilt up -- --enable_webhooks=false
# Or create tilt_config.json (gitignored) with: {"enable_webhooks": false}
config.define_bool("enable_webhooks", args=True,
    usage="Enable webhook support (default: true). Set false for controller-only development.")
cfg = config.parse()
ENABLE_WEBHOOKS = cfg.get("enable_webhooks", True)

CERT_MANAGER_VERSION = "v1.14.5"
WEBHOOK_NAMESPACE    = "spanner-autoscaler"
CERT_NAME            = "spanner-autoscaler-serving-cert"
WEBHOOK_CERT_DIR     = "bin/webhook-certs"
KUSTOMIZE            = "./bin/kustomize"

# ── Prerequisites ──────────────────────────────────────────────────────────────
# Install kustomize into ./bin if not present (runs once at Tilt load time).
local("test -f {k} || make kustomize".format(k=KUSTOMIZE), quiet=True, echo_off=True)

# ── Emulators (docker-compose) ─────────────────────────────────────────────────
# Spanner emulator (:9010 gRPC, :9011 admin) and
# Monitoring emulator (:9090 gRPC, :9091 admin) are managed by docker-compose.
docker_compose('./docker-compose.yml')

# ── Manifest generation ────────────────────────────────────────────────────────
# Re-runs make manifests generate when API types change.
local_resource(
    'generate',
    cmd='make manifests generate',
    deps=['api/'],
    ignore=['**/zz_generated.*.go'],
    labels=['setup'],
)

# ── cert-manager (webhook mode only) ──────────────────────────────────────────
if ENABLE_WEBHOOKS:
    local_resource(
        'cert-manager',
        cmd="""
            if ! kubectl get deployment -n cert-manager cert-manager-webhook > /dev/null 2>&1; then
                echo "Installing cert-manager {v}..."
                kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/{v}/cert-manager.yaml
            fi
            kubectl wait --for=condition=available --timeout=300s -n cert-manager deployment/cert-manager
            kubectl wait --for=condition=available --timeout=300s -n cert-manager deployment/cert-manager-webhook
            kubectl wait --for=condition=available --timeout=300s -n cert-manager deployment/cert-manager-cainjector
        """.format(v=CERT_MANAGER_VERSION),
        labels=['setup'],
    )

# ── Kubernetes resources ───────────────────────────────────────────────────────
if ENABLE_WEBHOOKS:
    # deploy-dev overlay: scales in-cluster controller to 0 and routes
    # webhook traffic to host.docker.internal:9443 (this machine).
    local_resource(
        'apply-manifests',
        cmd=KUSTOMIZE + ' build config/deploy-dev | kubectl apply -f -',
        deps=[
            'config/certmanager/',
            'config/crd/',
            'config/default/',
            'config/deploy-dev/',
            'config/manager/',
            'config/rbac/',
            'config/webhook/',
        ],
        resource_deps=['generate', 'cert-manager'],
        labels=['setup'],
    )
else:
    # No-webhook mode: apply CRDs only.
    k8s_yaml(local(KUSTOMIZE + ' build config/crd'))

# ── Webhook certificates ───────────────────────────────────────────────────────
if ENABLE_WEBHOOKS:
    local_resource(
        'dev-certs',
        cmd="""
            kubectl wait --for=condition=ready \
                certificate/{cert} \
                -n {ns} --timeout=120s
            make dev-certs
        """.format(cert=CERT_NAME, ns=WEBHOOK_NAMESPACE),
        labels=['setup'],
        resource_deps=['apply-manifests'],
    )

# ── Controller ─────────────────────────────────────────────────────────────────
EMULATOR_FLAGS = '--spanner-endpoint=localhost:9010 --metrics-endpoint=localhost:9090 --leader-elect=false'

# ── Emulator instance setup ────────────────────────────────────────────────────
# Create the beta-instance in the Spanner emulator and apply the local sample
# SpannerAutoscaler resource after emulators are ready.
local_resource(
    'setup-emulator-instance',
    cmd="""
        until curl -sf -X PUT http://localhost:9011/instances/beta-project/beta-instance \
            -H 'Content-Type: application/json' \
            -d '{"processing_units": 1000}' > /dev/null; do sleep 1; done
    """,
    resource_deps=['spanner-emulator'],
    labels=['setup'],
)

SAMPLE_FILES = [
    'config/samples/spanner_v1beta1_spannerautoscaler_local.yaml',
    'config/samples/spanner_v1beta1_spannerautoscaleschedule_local.yaml',
]
SAMPLE_APPLY_ARGS = ' '.join(['-f ' + f for f in SAMPLE_FILES])

if ENABLE_WEBHOOKS:
    # In webhook mode, retry until the webhook is ready to admit the resources.
    local_resource(
        'apply-sample',
        cmd="""
            until kubectl apply {args} 2>&1; do
                echo 'Retrying sample apply until webhook is ready...'
                sleep 3
            done
        """.format(args=SAMPLE_APPLY_ARGS),
        deps=SAMPLE_FILES,
        resource_deps=['controller', 'setup-emulator-instance'],
        labels=['setup'],
    )
else:
    # In no-webhook mode, manifests (including the namespace) are not applied via
    # apply-manifests, so ensure the namespace exists before applying the samples.
    local_resource(
        'ensure-namespace',
        cmd='kubectl create namespace {} --dry-run=client -o yaml | kubectl apply -f -'.format(WEBHOOK_NAMESPACE),
        labels=['setup'],
    )
    local_resource(
        'apply-sample',
        cmd='kubectl apply {args}'.format(args=SAMPLE_APPLY_ARGS),
        deps=SAMPLE_FILES,
        resource_deps=['controller', 'setup-emulator-instance', 'ensure-namespace'],
        labels=['setup'],
    )

if ENABLE_WEBHOOKS:
    local_resource(
        'controller',
        serve_cmd='go run ./cmd/main.go -zap-devel --cert-dir={} {}'.format(WEBHOOK_CERT_DIR, EMULATOR_FLAGS),
        deps=['cmd/', 'api/', 'internal/'],
        resource_deps=['dev-certs'],
        labels=['controller'],
    )
else:
    local_resource(
        'controller',
        serve_cmd='ENABLE_WEBHOOKS=false go run ./cmd/main.go -zap-devel {}'.format(EMULATOR_FLAGS),
        deps=['cmd/', 'api/', 'internal/'],
        labels=['controller'],
    )
