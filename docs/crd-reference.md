# API Reference

## Packages
- [spanner.mercari.com/v1beta1](#spannermercaricomv1beta1)


## spanner.mercari.com/v1beta1

Package v1beta1 contains API Schema definitions for the spanner v1beta1 API group

### Resource Types
- [SpannerAutoscaleSchedule](#spannerautoscaleschedule)
- [SpannerAutoscaler](#spannerautoscaler)
- [SpannerManualScaling](#spannermanualscaling)
- [SpannerManualScalingList](#spannermanualscalinglist)



#### ActiveManualScaling



A `SpannerManualScaling` which is currently overriding this autoscaler's
processing units, if any. Used as a convenience field on
`SpannerAutoscaler.status` so that the active override is visible without
listing SpannerManualScaling resources separately.



_Appears in:_
- [SpannerAutoscalerStatus](#spannerautoscalerstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the `SpannerManualScaling` resource in the same namespace. |  |  |
| `processingUnits` _integer_ | ProcessingUnits being targeted. When a step size is set on the source,<br />this is the final target — not the value currently applied to the<br />Spanner instance (use SpannerAutoscaler.status.currentProcessingUnits<br />for that). |  |  |
| `ramp` _boolean_ | Ramp is true when the source has either `scaleupStepSize` or<br />`scaledownStepSize` set, indicating step-bounded pacing rather than a<br />single-jump override. Derived from the source spec for convenience in<br />printer columns and dashboards. (Interval-only specification is<br />treated as single-jump.) |  |  |
| `expiresAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | ExpiresAt, if specified on the source resource. |  | Optional: \{\} <br /> |


#### ActiveSchedule



A `SpannerAutoscaleSchedule` which is currently active and will be used for calculating the autoscaling range.



_Appears in:_
- [SpannerAutoscalerStatus](#spannerautoscalerstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the `SpannerAutoscaleSchedule` |  |  |
| `endTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | The time until when this schedule will remain active |  |  |
| `additionalPU` _integer_ | The extra compute capacity which will be added because of this schedule |  |  |


#### AuthType

_Underlying type:_ _string_

Type for specifying authentication methods

_Validation:_
- Enum: [gcp-sa-key impersonation adc]

_Appears in:_
- [Authentication](#authentication)

| Field | Description |
| --- | --- |
| `gcp-sa-key` |  |
| `impersonation` |  |
| `adc` |  |


#### Authentication



Authentication details for the Spanner instance



_Appears in:_
- [SpannerAutoscalerSpec](#spannerautoscalerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[AuthType](#authtype)_ | Authentication method to be used for GCP authentication.<br />If `ImpersonateConfig` as well as `IAMKeySecret` is nil, this will be set to use ADC be default. |  | Enum: [gcp-sa-key impersonation adc] <br /> |
| `impersonateConfig` _[ImpersonateConfig](#impersonateconfig)_ | Details of the GCP service account which will be impersonated, for authentication to GCP.<br />This can used only on GKE clusters, when workload identity is enabled.<br />[[Ref](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity)].<br />This is a pointer because structs with string slices can not be compared for zero values |  |  |
| `iamKeySecret` _[IAMKeySecret](#iamkeysecret)_ | Details of the k8s secret which contains the GCP service account authentication key (in JSON).<br />[[Ref](https://cloud.google.com/kubernetes-engine/docs/tutorials/authenticating-to-cloud-platform)].<br />This is a pointer because structs with string slices can not be compared for zero values |  |  |




#### CPUMetricType

_Underlying type:_ _string_

CPUMetricType identifies which Cloud Monitoring CPU metric is being used
for autoscaling decisions.

_Validation:_
- Enum: [HighPriority Total Both]

_Appears in:_
- [SpannerAutoscalerStatus](#spannerautoscalerstatus)

| Field | Description |
| --- | --- |
| `HighPriority` | CPUMetricTypeHighPriority uses spanner.googleapis.com/instance/cpu/utilization_by_priority<br />with priority=high filter.<br /> |
| `Total` | CPUMetricTypeTotal uses spanner.googleapis.com/instance/cpu/utilization (all priorities).<br /> |
| `Both` | CPUMetricTypeBoth indicates that both highPriority and total metrics are being synced<br />simultaneously (dual CPU scaling mode).<br /> |


#### ComputeType

_Underlying type:_ _string_

Type for specifying compute capacity categories

_Validation:_
- Enum: [nodes processing-units]

_Appears in:_
- [ScaleConfig](#scaleconfig)

| Field | Description |
| --- | --- |
| `nodes` |  |
| `processing-units` |  |


#### IAMKeySecret



Details of the secret which has the GCP service account key for authentication



_Appears in:_
- [Authentication](#authentication)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the secret which contains the authentication key |  |  |
| `namespace` _string_ | Namespace of the secret which contains the authentication key |  |  |
| `key` _string_ | Name of the yaml 'key' under which the authentication value is stored |  |  |


#### ImpersonateConfig



Details of the impersonation service account for GCP authentication



_Appears in:_
- [Authentication](#authentication)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetServiceAccount` _string_ | The service account which will be impersonated |  |  |
| `delegates` _string array_ | Delegation chain for the service account impersonation.<br />[[Ref](https://pkg.go.dev/google.golang.org/api/impersonate#hdr-Required_IAM_roles)] |  |  |


#### InstanceState

_Underlying type:_ _string_





_Appears in:_
- [SpannerAutoscalerStatus](#spannerautoscalerstatus)

| Field | Description |
| --- | --- |
| `unspecified` |  |
| `creating` | The instance is still being created. Resources may not be<br />available yet, and operations such as database creation may not<br />work.<br /> |
| `ready` | The instance is fully created and ready to do work such as<br />creating databases.<br /> |




#### ScaleConfig



Details of the autoscaling parameters for the Spanner instance



_Appears in:_
- [SpannerAutoscalerSpec](#spannerautoscalerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `computeType` _[ComputeType](#computetype)_ | Whether to use `nodes` or `processing-units` for scaling.<br />This is only used at the time of CustomResource creation. If compute capacity is provided in `nodes`, then it is automatically converted to `processing-units` at the time of resource creation, and internally, only `ProcessingUnits` are used for computations and scaling. |  | Enum: [nodes processing-units] <br /> |
| `nodes` _[ScaleConfigNodes](#scaleconfignodes)_ | If `nodes` are provided at the time of resource creation, then they are automatically converted to `processing-units`. So it is recommended to use only the processing units. Ref: [Spanner Compute Capacity](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity) |  |  |
| `processingUnits` _[ScaleConfigPUs](#scaleconfigpus)_ | ProcessingUnits for scaling of the Spanner instance. Ref: [Spanner Compute Capacity](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity) |  |  |
| `scaledownStepSize` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#intorstring-intstr-util)_ | The maximum number of processing units which can be deleted in one scale-down operation. It can be a multiple of 100 for values < 1000, or a multiple of 1000 otherwise.<br />It can also be a percentage of the total number of processing units at the start of the scale-down operation. | 2000 |  |
| `scaledownInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#duration-v1-meta)_ | How often autoscaler is reevaluated for scale down.<br />The cool down period between two consecutive scaledown operations. If this option is omitted, the value of the `--scale-down-interval` command line option is taken as the default value. |  |  |
| `scaleupStepSize` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#intorstring-intstr-util)_ | The maximum number of processing units which can be added in one scale-up operation. It can be a multiple of 100 for values < 1000, or a multiple of 1000 otherwise.<br />It can also be a percentage of the total number of processing units at the start of the scale-up operation. | 0 |  |
| `scaleupInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#duration-v1-meta)_ | How often autoscaler is reevaluated for scale up.<br />The warm up period between two consecutive scaleup operations. If this option is omitted, the value of the `--scale-up-interval` command line option is taken as the default value. |  |  |
| `scaledownAllowedTimes` _string array_ | Scale down is allowed only during the time periods specified in standard cron format.<br />Multiple cron expressions can be specified to handle complex time ranges including periods that cross midnight.<br />If not specified, scale down is allowed at any time.<br />Examples:<br />  - ["* 2-4 * * *"] allows scale down from 2:00 AM to 4:59 AM daily<br />  - ["* 23 * * *", "* 0-5 * * *"] allows scale down from 11:00 PM to 5:59 AM daily (crossing midnight) |  | Optional: \{\} <br /> |
| `scaledownNotAllowedTimes` _string array_ | Scale down is NOT allowed during the time periods specified in standard cron format.<br />Multiple cron expressions can be specified to handle complex time ranges including periods that cross midnight.<br />If not specified, scale down is allowed at any time (unless scaledownAllowedTimes is specified).<br />Cannot be used together with scaledownAllowedTimes - only one of the two fields can be specified.<br />Examples:<br />  - ["* 9-17 * * 1-5"] prevents scale down during business hours (9:00 AM to 5:59 PM on weekdays)<br />  - ["* 12-13 * * 1-5", "* 18-19 * * 1-5"] prevents scale down during lunch and evening peak hours |  | Optional: \{\} <br /> |
| `targetCPUUtilization` _[TargetCPUUtilization](#targetcpuutilization)_ | The CPU utilization which the autoscaling will try to achieve. Ref: [Spanner CPU utilization](https://cloud.google.com/spanner/docs/cpu-utilization#task-priority) |  |  |


#### ScaleConfigNodes



Compute capacity in terms of Nodes



_Appears in:_
- [ScaleConfig](#scaleconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `min` _integer_ | Minimum number of Nodes for the autoscaling range |  |  |
| `max` _integer_ | Maximum number of Nodes for the autoscaling range |  |  |


#### ScaleConfigPUs



Compute capacity in terms of Processing Units



_Appears in:_
- [ScaleConfig](#scaleconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `min` _integer_ | Minimum number of Processing Units for the autoscaling range |  | MultipleOf: 100 <br /> |
| `max` _integer_ | Maximum number of Processing Units for the autoscaling range |  | MultipleOf: 100 <br /> |


#### Schedule



The recurring frequency and the length of time for which a schedule will remain active



_Appears in:_
- [SpannerAutoscaleScheduleSpec](#spannerautoscaleschedulespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cron` _string_ | The recurring frequency of the schedule in [standard cron](https://en.wikipedia.org/wiki/Cron) format.<br />Extended syntax (`L`, `L-n`, `nW`, `LW`, `DAY#n`, `DAY#L`) is also supported — see [go-cron Extended Syntax](https://pkg.go.dev/github.com/netresearch/go-cron#hdr-Extended_Syntax__Optional_).<br />Examples and verification utility: https://crontab.guru |  |  |
| `duration` _string_ | The length of time for which this schedule will remain active each time the cron is triggered. |  |  |


#### SpannerAutoscaleSchedule



SpannerAutoscaleSchedule is the Schema for the spannerautoscaleschedules API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `spanner.mercari.com/v1beta1` | | |
| `kind` _string_ | `SpannerAutoscaleSchedule` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SpannerAutoscaleScheduleSpec](#spannerautoscaleschedulespec)_ |  |  |  |
| `status` _[SpannerAutoscaleScheduleStatus](#spannerautoscaleschedulestatus)_ |  |  |  |


#### SpannerAutoscaleScheduleSpec



SpannerAutoscaleScheduleSpec defines the desired state of SpannerAutoscaleSchedule



_Appears in:_
- [SpannerAutoscaleSchedule](#spannerautoscaleschedule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetResource` _string_ | The `SpannerAutoscaler` resource name with which this schedule will be registered.<br />Immutable after creation. |  |  |
| `additionalProcessingUnits` _integer_ | The extra compute capacity which will be added when this schedule is active. |  |  |
| `schedule` _[Schedule](#schedule)_ | The details of when and for how long this schedule will be active. |  |  |


#### SpannerAutoscaleScheduleStatus



SpannerAutoscaleScheduleStatus defines the observed state of SpannerAutoscaleSchedule



_Appears in:_
- [SpannerAutoscaleSchedule](#spannerautoscaleschedule)



#### SpannerAutoscaler



SpannerAutoscaler is the Schema for the spannerautoscalers API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `spanner.mercari.com/v1beta1` | | |
| `kind` _string_ | `SpannerAutoscaler` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SpannerAutoscalerSpec](#spannerautoscalerspec)_ |  |  |  |
| `status` _[SpannerAutoscalerStatus](#spannerautoscalerstatus)_ |  |  |  |


#### SpannerAutoscalerSpec



SpannerAutoscalerSpec defines the desired state of SpannerAutoscaler



_Appears in:_
- [SpannerAutoscaler](#spannerautoscaler)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetInstance` _[TargetInstance](#targetinstance)_ | The Spanner instance which will be managed for autoscaling |  |  |
| `authentication` _[Authentication](#authentication)_ | Authentication details for the Spanner instance |  |  |
| `scaleConfig` _[ScaleConfig](#scaleconfig)_ | Details of the autoscaling parameters for the Spanner instance |  |  |


#### SpannerAutoscalerStatus



SpannerAutoscalerStatus defines the observed state of SpannerAutoscaler



_Appears in:_
- [SpannerAutoscaler](#spannerautoscaler)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schedules` _string array_ | List of schedules which are registered with this spanner-autoscaler instance |  |  |
| `currentlyActiveSchedules` _[ActiveSchedule](#activeschedule) array_ | List of all the schedules which are currently active and will be used in calculating compute capacity |  |  |
| `lastScaleTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | Last time the `SpannerAutoscaler` scaled the number of Spanner nodes.<br />Used by the autoscaler to control how often the number of nodes are changed |  |  |
| `lastSyncTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | Last time the `SpannerAutoscaler` fetched and synced the metrics from Spanner |  |  |
| `currentProcessingUnits` _integer_ | Current number of processing-units in the Spanner instance |  |  |
| `desiredProcessingUnits` _integer_ | Desired number of processing-units in the Spanner instance |  |  |
| `desiredMinPUs` _integer_ | Minimum number of processing units based on the currently active schedules |  |  |
| `desiredMaxPUs` _integer_ | Maximum number of processing units based on the currently active schedules |  |  |
| `instanceState` _[InstanceState](#instancestate)_ | State of the Cloud Spanner instance |  |  |
| `currentHighPriorityCPUUtilization` _integer_ | Current average CPU utilization for high priority task, represented as a percentage.<br />In dual CPU scaling mode (both highPriority and total configured), this value is<br />fetched concurrently with currentTotalCPUUtilization. Because Cloud Monitoring<br />ingests the underlying metrics (utilization_by_priority and utilization) independently,<br />the two fields may briefly reflect different alignment windows and the relation<br />currentTotalCPUUtilization >= currentHighPriorityCPUUtilization is not guaranteed<br />on every sync. The autoscaling decision uses both values independently and takes<br />the larger required processing units, so the transient inconsistency does not cause<br />over-scaling down. |  |  |
| `currentTotalCPUUtilization` _integer_ | Current total CPU utilization (all priorities), represented as a percentage.<br />This field is populated only when spec.scaleConfig.targetCPUUtilization.total is specified.<br />See the note on currentHighPriorityCPUUtilization for the consistency caveat in dual mode. |  |  |
| `currentCPUMetricType` _[CPUMetricType](#cpumetrictype)_ | CurrentCPUMetricType is the CPU metric type that was used in the last sync cycle.<br />The controller uses this to detect metric-type switches and skip scaling until<br />the status reflects the newly configured metric type. |  | Enum: [HighPriority Total Both] <br /> |
| `activeManualScaling` _[ActiveManualScaling](#activemanualscaling)_ | ActiveManualScaling references the `SpannerManualScaling` resource that<br />is currently overriding this autoscaler's processing units. Empty when<br />normal (CPU- and schedule-driven) autoscaling is in effect. |  | Optional: \{\} <br /> |


#### SpannerManualScaling



SpannerManualScaling is the Schema for the spannermanualscalings API.



_Appears in:_
- [SpannerManualScalingList](#spannermanualscalinglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `spanner.mercari.com/v1beta1` | | |
| `kind` _string_ | `SpannerManualScaling` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SpannerManualScalingSpec](#spannermanualscalingspec)_ |  |  |  |
| `status` _[SpannerManualScalingStatus](#spannermanualscalingstatus)_ |  |  |  |


#### SpannerManualScalingList



SpannerManualScalingList contains a list of SpannerManualScaling.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `spanner.mercari.com/v1beta1` | | |
| `kind` _string_ | `SpannerManualScalingList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[SpannerManualScaling](#spannermanualscaling) array_ |  |  |  |


#### SpannerManualScalingPhase

_Underlying type:_ _string_

SpannerManualScalingPhase indicates the lifecycle stage of a manual
override.



_Appears in:_
- [SpannerManualScalingStatus](#spannermanualscalingstatus)

| Field | Description |
| --- | --- |
| `Pending` | SpannerManualScalingPhasePending: created but not yet picked up by the<br />controller. Transient (typically resolved on the first reconcile).<br /> |
| `Progressing` | SpannerManualScalingPhaseProgressing: the step-size field for the<br />required direction is set and CurrentProcessingUnits has not yet<br />reached spec.processingUnits. The controller is taking step-bounded,<br />interval-gated updates toward the target. A single-jump override<br />(step size unset) skips this phase and goes Pending -> Active.<br /> |
| `Active` | SpannerManualScalingPhaseActive: CurrentProcessingUnits equals<br />spec.processingUnits and the override is holding the target on the<br />parent SpannerAutoscaler.<br /> |
| `Expired` | SpannerManualScalingPhaseExpired: ExpiresAt has elapsed; no longer<br />pinning PU. With a step size set, this can occur mid-ramp; in that<br />case the target may not have been reached.<br /> |
| `Superseded` | SpannerManualScalingPhaseSuperseded: a newer SpannerManualScaling for<br />the same targetResource was created and is now active.<br /> |
| `Invalid` | SpannerManualScalingPhaseInvalid: the targetResource does not exist<br />(in the same namespace) or the override otherwise cannot be applied.<br />The resource is retained so the operator can fix the cause; the<br />controller transitions phase back to Pending/Active if the cause is<br />resolved.<br /> |


#### SpannerManualScalingSpec



SpannerManualScalingSpec defines a one-shot operation that drives the parent
SpannerAutoscaler's processing units toward a target value, bypassing
CPU-based autoscaling, SpannerAutoscaleSchedule additions, and the
scaledownAllowedTimes / scaledownNotAllowedTimes windows for as long as it
is active.

Pacing is inferred from the presence of the step-size field for the required
direction:

  - ScaleupStepSize unset (when target > current)  → single-jump scale-up.
  - ScaleupStepSize set    (when target > current) → stepped ramp; the
    cadence comes from ScaleupInterval when set, otherwise from the
    controller's --scale-up-interval flag default.
  - ScaledownStepSize / ScaledownInterval behave symmetrically.

Interval-only specification (e.g. ScaleupInterval set but ScaleupStepSize
unset) has no effect; the validating webhook emits an admission warning to
flag the likely typo.

Spec is IMMUTABLE after creation: the validating webhook rejects any spec
mutation. To change the target PU, ramp parameters, or extend ExpiresAt,
create a new SpannerManualScaling — the newest-creationTimestamp rule
atomically supersedes the older resource with no autoscaling gap. To
shorten / force expiration, kubectl delete the resource.



_Appears in:_
- [SpannerManualScaling](#spannermanualscaling)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetResource` _string_ | The `SpannerAutoscaler` resource name (in the same namespace) this<br />override applies to. Immutable after creation. |  |  |
| `processingUnits` _integer_ | ProcessingUnits is the target value that this override drives toward<br />while active. Must be a multiple of 100 (for values < 1000) or a<br />multiple of 1000. |  | Minimum: 100 <br /> |
| `scaleupStepSize` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#intorstring-intstr-util)_ | ScaleupStepSize, when set, activates stepped scale-up: each reconcile<br />applies at most this delta until ProcessingUnits is reached. Accepts the<br />same int-or-percent format as the parent SpannerAutoscaler's<br />scaleConfig.scaleupStepSize. Unset means single-jump scale-up — the<br />override jumps the full upward delta in one reconcile.<br />Presence of this field (when target > current) is the sole signal that<br />gates stepped vs single-jump scale-up. |  | Optional: \{\} <br /> |
| `scaleupInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#duration-v1-meta)_ | ScaleupInterval is the minimum wall-clock time the controller waits<br />between successive upward steps. Only consulted when ScaleupStepSize is<br />set. Unset falls back to the controller's --scale-up-interval flag<br />value.<br />Setting ScaleupInterval without ScaleupStepSize has no effect; the<br />validating webhook emits an admission warning for that combination. |  | Optional: \{\} <br /> |
| `scaledownStepSize` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#intorstring-intstr-util)_ | ScaledownStepSize mirrors ScaleupStepSize for the downward direction<br />(ProcessingUnits < CurrentProcessingUnits). |  | Optional: \{\} <br /> |
| `scaledownInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#duration-v1-meta)_ | ScaledownInterval mirrors ScaleupInterval for the downward direction.<br />Unset falls back to the controller's --scale-down-interval flag value. |  | Optional: \{\} <br /> |
| `expiresAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | ExpiresAt is the time after which this override is automatically<br />deactivated. When omitted, the override remains active until the<br />resource is deleted.<br />Accepts any RFC 3339 timestamp with a timezone designator (Z, +09:00,<br />-08:00, etc.). Per Kubernetes convention (metav1.Time), the value is<br />normalized to UTC for storage in etcd and for kubectl get/describe<br />output; the absolute instant in time is preserved.<br />When a step size is set, the controller does not guarantee that<br />ProcessingUnits is reached before ExpiresAt — it is the user's<br />responsibility to size ExpiresAt against ScaleupStepSize /<br />ScaleupInterval (or the scaledown counterparts) so the ramp fits in the<br />window. If ExpiresAt elapses mid-ramp, the override is deactivated<br />wherever CurrentProcessingUnits happens to be and normal autoscaling<br />resumes from there. |  | Optional: \{\} <br /> |


#### SpannerManualScalingStatus



SpannerManualScalingStatus defines the observed state of
SpannerManualScaling.



_Appears in:_
- [SpannerManualScaling](#spannermanualscaling)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[SpannerManualScalingPhase](#spannermanualscalingphase)_ | Phase reflects the lifecycle stage of this override. |  | Optional: \{\} <br /> |
| `appliedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | AppliedAt is the time the controller first applied this override to<br />the target. |  | Optional: \{\} <br /> |
| `reachedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | ReachedAt is the time CurrentProcessingUnits first matched<br />spec.processingUnits while this override was active. Set only when the<br />target has been reached; remains nil for stepped ramps still in<br />progress. |  | Optional: \{\} <br /> |
| `currentProcessingUnits` _integer_ | CurrentProcessingUnits is the parent SpannerAutoscaler's PU as last<br />observed by this controller. Useful for stepped ramps to see progress<br />toward spec.processingUnits without a cross-resource lookup. |  | Optional: \{\} <br /> |
| `finishedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#time-v1-meta)_ | FinishedAt is the time this resource first transitioned to a terminal<br />phase (Expired, Superseded, or Invalid). Used as the sort key by the<br />controller's history-limit GC. Set once and not updated thereafter<br />(terminal phases do not transition back). |  | Optional: \{\} <br /> |
| `message` _string_ | Message is a human-readable explanation, especially when Phase is<br />Invalid or when Expired occurred mid-ramp (target not reached). |  | Optional: \{\} <br /> |


#### TargetCPUUtilization







_Appears in:_
- [ScaleConfig](#scaleconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `highPriority` _integer_ | Desired CPU utilization for 'High Priority' CPU consumption category. Ref: [Spanner CPU utilization](https://cloud.google.com/spanner/docs/cpu-utilization#task-priority)<br />Optional. When specified together with 'total', scale-out occurs when either threshold is exceeded (OR condition).<br />At least one of 'highPriority' or 'total' must be specified. |  | ExclusiveMaximum: true <br />ExclusiveMinimum: true <br />Maximum: 100 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `total` _integer_ | Desired total CPU utilization (all priorities combined). Ref: [Spanner CPU utilization](https://cloud.google.com/spanner/docs/cpu-utilization)<br />Optional. When specified together with 'highPriority', scale-out occurs when either threshold is exceeded (OR condition).<br />At least one of 'highPriority' or 'total' must be specified. |  | ExclusiveMaximum: true <br />ExclusiveMinimum: true <br />Maximum: 100 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### TargetInstance



The Spanner instance which will be managed for autoscaling



_Appears in:_
- [SpannerAutoscalerSpec](#spannerautoscalerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `projectId` _string_ | The GCP Project id of the Spanner instance |  |  |
| `instanceId` _string_ | The instance id of the Spanner instance |  |  |


