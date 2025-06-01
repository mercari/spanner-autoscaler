# API Reference

## Packages
- [spanner.mercari.com/v1beta1](#spannermercaricomv1beta1)


## spanner.mercari.com/v1beta1

Package v1beta1 contains API Schema definitions for the spanner v1beta1 API group

### Resource Types
- [SpannerAutoscaleSchedule](#spannerautoscaleschedule)
- [SpannerAutoscaler](#spannerautoscaler)



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
| `scaleupStepSize` _integer_ | The maximum number of processing units which can be added in one scale-up operation. It can be a multiple of 100 for values < 1000, or a multiple of 1000 otherwise. | 0 |  |
| `scaleupInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#duration-v1-meta)_ | How often autoscaler is reevaluated for scale up.<br />The warm up period between two consecutive scaleup operations. If this option is omitted, the value of the `--scale-up-interval` command line option is taken as the default value. |  |  |
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
| `cron` _string_ | The recurring frequency of the schedule in [standard cron](https://en.wikipedia.org/wiki/Cron) format. Examples and verification utility: https://crontab.guru |  |  |
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
| `targetResource` _string_ | The `SpannerAutoscaler` resource name with which this schedule will be registered |  |  |
| `additionalProcessingUnits` _integer_ | The extra compute capacity which will be added when this schedule is active |  |  |
| `schedule` _[Schedule](#schedule)_ | The details of when and for how long this schedule will be active |  |  |


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
| `currentHighPriorityCPUUtilization` _integer_ | Current average CPU utilization for high priority task, represented as a percentage |  |  |


#### TargetCPUUtilization







_Appears in:_
- [ScaleConfig](#scaleconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `highPriority` _integer_ | Desired CPU utilization for 'High Priority' CPU consumption category. Ref: [Spanner CPU utilization](https://cloud.google.com/spanner/docs/cpu-utilization#task-priority) |  | ExclusiveMaximum: true <br />ExclusiveMinimum: true <br />Maximum: 100 <br />Minimum: 0 <br /> |


#### TargetInstance



The Spanner instance which will be managed for autoscaling



_Appears in:_
- [SpannerAutoscalerSpec](#spannerautoscalerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `projectId` _string_ | The GCP Project id of the Spanner instance |  |  |
| `instanceId` _string_ | The instance id of the Spanner instance |  |  |


