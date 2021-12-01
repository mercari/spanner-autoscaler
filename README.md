# Spanner Autoscaler

[![actions-workflow-test][actions-workflow-test-badge]][actions-workflow-test]
[![release][release-badge]][release]
[![license][license-badge]][license]

Spanner Autoscaler is a [Kubernetes Operator](https://coreos.com/operators/) to scale [Google Cloud Spanner](https://cloud.google.com/spanner/) automatically based on Cloud Spanner Instance CPU utilization like [Horizontal Pod Autoscaler](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/).

## Overview

[Cloud Spanner](https://cloud.google.com/spanner) is scalable.
When CPU utilization gets high, we can [reduce CPU utilization by increasing compute capacity](https://cloud.google.com/spanner/docs/cpu-utilization?hl=en#add-compute-capacity).

Spanner Autoscaler is created to reconcile Cloud Spanner compute capacity like [Horizontal Pod Autoscaler](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/) by configuring `minNodes`, `maxNodes`, and `targetCPUUtilization`.

<img src="./docs/assets/overview.jpg" width="450" height="300">

When CPU Utilization(High Priority) is above `targetCPUUtilization`, Spanner Autoscaler calculates desired compute capacity and increases compute capacity.

<img src="./docs/assets/cpu_utilization.png" width="400" height="200"> <img src="./docs/assets/node_scaleup.png" width="400" height="200">

The [pricing of Cloud Spanner](https://cloud.google.com/spanner/pricing) states that any compute capacity which is provisioned will be billed for a minimum of one hour, so Spanner Autoscaler maintains the increased compute capacity for about an hour. Spanner Autoscaler has `--scale-down-interval` flag (default: 55min) for achieving this.

While scaling down, if Spanner Autoscaler reduces a lot of compute capacity at once like 10000 PU -> 1000 PU, it will cause a latency increase. Spanner Autoscaler decreases the compute capacity in steps to avoid such large disruptions. This step size can be provided with the `maxScaleDownNodes` parameter (default: 2).
<img src="./docs/assets/node_scaledown.png" width="400" height="200">


## Installation

Spanner Autoscaler can be installed using [KPT](https://kpt.dev/installation/) by following 2 steps:

1. Deploy the operator through `kpt`

   ```console
   $ kpt pkg get https://github.com/mercari/spanner-autoscaler/config spanner-autoscaler
   $ kpt live init spanner-autoscaler/kpt
   $ kpt live install-resource-group

   ## Append '--dry-run' to the below line to just
   ## check the resources which will be created
   $ kustomize build spanner-autoscaler/kpt | kpt live apply -

   ## To uninstall, use the following
   $ kustomize build spanner-autoscaler/kpt | kpt live destroy -
   ```
   > :information_source: **TIP:** Instead of `kpt`, you can also use `kubectl` directly to apply the resources with
   >   ```console
   >   $ kustomize build spanner-autoscaler/default | kubectl apply -f -
   >   ```
   > These resources can then be adopted by `kpt` by using the `--inventory-policy=adopt` flag while using `kpt live apply` command. [More info](https://kpt.dev/reference/cli/live/apply/?id=flags).

1. Create a Custom Resource for managing a spanner instance

   ```console
   $ kubectl apply -f spanner-autoscaler/samples
   ```
   Examples of CRDs can be found [below](#examples).\
   For authentication using a GCP service account JSON key, follow [these steps](#gcp-setup) to create a k8s secret with credentials.


## `SpannerAutoscaler` CRD reference

Following is a reference of the parameters which can be provided in the `spec` section of the `SpannerAutoscaler` CRD:

Parameter | Type | Required | Description
--- | --- | --- | ---
`scaleTargetRef` | object | yes | Spanner Instance which will be auto scaled
`scaleTargetRef.projectId` | string | yes | GCP Project ID
`scaleTargetRef.instanceId` | string | yes | Cloud Spanner Instance ID
`serviceAccountSecretRef` | object | no | Secret created [here](#authenticate-with-service-account-json-key)
`serviceAccountSecretRef.name` | string | yes | Name of the k8s secret
`serviceAccountSecretRef.namespace` | string | yes | Namespace of the k8s secret
`serviceAccountSecretRef.key` | string | yes | Name of the key in the secret which holds the authentication information
`impersonateConfig` | object | no | Impersonation config
`impersonateConfog.targetServiceAccount` | string | yes | Email address of the service account to impersonate ([`GSA_SPANNER`](#using-service-accounts-with-workload-identity-and-impersonation))
`impersonateConfog.delegates` | list of string | yes | List of target service account emails in a delegation chain ([Ref](https://pkg.go.dev/google.golang.org/api/impersonate#CredentialsConfig))
`minProcessingUnits` | integer | no | Minimum [processing units](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity)
`maxProcessingUnits` | integer | no | Maximum [processing units](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity)
`minNodes` | integer | no | Equals [`minProcessingUnits / 1000`](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity)
`maxNodes` | integer | no | Equals [`maxProcessingUnits / 1000`](https://cloud.google.com/spanner/docs/compute-capacity#compute_capacity)
`maxScaleDownNodes` | integer | no | Maximum number of nodes to remove in one scale-down cycle
`targetCPUUtilization` | object | yes | Spanner [CPU utilization metrics](https://cloud.google.com/spanner/docs/cpu-utilization)
`targetCPUUtilization.highPriority` | integer | yes | High Priority CPU Utilization value


## Examples

#### Single Service Account using Workload Identity:

```yaml
apiVersion: spanner.mercari.com/v1alpha1
kind: SpannerAutoscaler
metadata:
  name: spannerautoscaler-sample
  namespace: your-namespace
spec:
  scaleTargetRef:
    projectId: your-gcp-project-id
    instanceId: your-spanner-instance-id
  minNodes: 1
  maxNodes: 4
  maxScaleDownNodes: 1
  targetCPUUtilization:
    highPriority: 60
```

#### Using Service Account JSON key for each `SpannerAutoscaler`:

```diff
  apiVersion: spanner.mercari.com/v1alpha1
  kind: SpannerAutoscaler
  metadata:
    name: spannerautoscaler-sample
    namespace: your-namespace
  spec:
    scaleTargetRef:
      projectId: your-gcp-project-id
      instanceId: your-spanner-instance-id
+   serviceAccountSecretRef:
+     namespace: your-namespace
+     name: spanner-autoscaler-gcp-sa
+     key: service-account
    minNodes: 1
    maxNodes: 4
    maxScaleDownNodes: 1
    targetCPUUtilization:
      highPriority: 60
```

#### Using Service Accounts with Workload Identity and impersonation:

```diff
  apiVersion: spanner.mercari.com/v1alpha1
  kind: SpannerAutoscaler
  metadata:
    name: spannerautoscaler-sample
    namespace: your-namespace
  spec:
    scaleTargetRef:
      projectId: your-gcp-project-id
      instanceId: your-spanner-instance-id
+   impersonateConfig:
+     targetServiceAccount: GSA_SPANNER@TENANT_PROJECT.iam.gserviceaccount.com
    minNodes: 1
    maxNodes: 4
    maxScaleDownNodes: 1
    targetCPUUtilization:
      highPriority: 60
```


## GCP Setup

On your GCP project, you will need to enable `spanner.googleapis.com` and `monitoring.googleapis.com` APIs.

### Create service account

You will need to create at least one GCP service account, which will be used by the spanner-autoscaler controller to authenticate with GCP for modifying compute capacity of a Spanner instance. This service account should have the following roles:
  - `roles/spanner.admin` (on the Spanner instances)
  - `roles/monitoring.viewer` (on the project)

For fine grained access control, you should create one GCP service account per Spanner instance. This way, you will be able to specify a different service account in each of `SpannerAutoscaler` CRD resources you create later.

### Authenticate with service account JSON key

Generate a JSON key for the GCP service account (created [above](#create-service-account)) and put it in a Kubernetes Secret:
```sh
$ kubectl create secret generic spanner-autoscaler-gcp-sa --from-file=service-account=./service-account-key.json -n your-namespace
```
> :information_source: By default, `spanner-autoscaler` will have read access to `secret`s named `spanner-autoscaler-gcp-sa` in any namespace. If you wish to use a different name for your secret, then you need to explicitly create a `Role` and a `RoleBinding` ([example](/config/samples/rbac/role.yaml)) in your namespace. This will provide `spanner-autoscaler` with read access to any secret of your choice.

You can then refer to this secret in your `SpannerAutoscaler` CRD resource with `serviceAccountSecretRef` field [[example](#using-service-account-json-key-for-each-spannerautoscaler)].


### [Optional] Advanced methods for GCP authentication


Following are some other advanced methods which can also be used for GCP authentication:
<details> <summary>Details</summary>
<ul>

  #### Enable Workload Identity

  <details> <summary>Details</summary>

  You can configure the controller (`spanner-autoscaler-controller-manager`) to use [GKE Workload Identity](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity) feature for key-less GCP access. Steps to do this:
  1. Enable Workload Identity on the GKE cluster - [Ref](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity?hl=en#enable_on_cluster).
  1. Let's call the Kubernetes service account of the controller (`spanner-autoscaler/spanner-autoscaler-controller-manager`) as `KSA_CONTROLLER` and the GCP service account created [above](#create-service-account) as `GSA_CONTROLLER`.\
     Now configure Workload Identity between `KSA_CONTROLLER` and `GSA_CONTROLLER` with the following steps:
     1. Allow `KSA_CONTROLLER` to impersonate `GSA_CONTROLLER` by creating an IAM Policy binding:
        ```console
        $ gcloud iam service-accounts add-iam-policy-binding --role roles/iam.workloadIdentityUser --member "serviceAccount:PROJECT_ID.svc.id.goog[spanner-autoscaler/spanner-autoscaler-controller-manager]" GSA_CONTROLLER@PROJECT_ID.iam.gserviceaccount.com`
        ```
     1. Add annotation
        ```sh
        $ kubectl annotate serviceaccount  --namespace spanner-autoscaler spanner-autoscaler-controller-manager iam.gke.io/gcp-service-account=GSA_CONTROLLER@PROJECT_ID.iam.gserviceaccount.com`
        ```
  </details>
</ul>

<ul>

  #### Single service account with Workload Identity

  <details> <summary>Details</summary>

  The Kubernetes service account which is used for running the spanner-autoscaler controller can be bound to the GCP service account (created [above](#create-service-account)) through Workload Identity. If this is done, there is no need to provide `serviceAccountSecretRef` or `impersonateConfig` authentication parameters in the `spec` section of the `SpannerAutoscaler` CRD resources.

  An example for this is shown [here](#single-service-account-using-workload-identity).

  </details>
</ul>

<ul>

  #### Using service accounts with Workload Identity and Impersonation

  <details> <summary>Details</summary>

  In this method there are 3 service accounts involved (2 GCP service accounts and 1 Kubernetes service account):
  - `GSA_SPANNER`: The GCP Service Account (created [above](#create-service-account)) which has the correct permissions for modifying Spanner compute capacity
  - `GSA_CONTROLLER`: The GCP Service Account which is used for Workload Identity with the GKE cluster
  - `KSA_CONTROLLER`: The Kubernetes Service Account which is used for running the spanner-autoscaler controller pod in the GKE

  After enabling Workload Identity between `GSA_CONTROLLER` and `KSA_CONTROLLER`, you can configure `GSA_CONTROLLER` as `roles/iam.serviceAccountTokenCreator` of the `GSA_SPANNER` service account as follows:

  ```sh
  $ gcloud iam service-accounts add-iam-policy-binding $GSA_SPANNER --member=serviceAccount:$GSA_CONTROLLER --role=roles/iam.serviceAccountTokenCreator
  ```
  This will allow `KSA_CONTROLLER` to use `GSA_CONTROLLER` and impersonate (act as) `GSA_SPANNER` for a short period of time (by using a short-lived token). An example for this can be found [here](#using-service-accounts-with-workload-identity-and-impersonation).

  </details>
</ul>

<ul>

  **TIP:** Custom role with minimum permissions

  <details> <summary>Details</summary>

  Instead of predefined roles, you can define and use a [custom role](https://cloud.google.com/iam/docs/creating-custom-roles/?hl=en) with lesser privileges for Spanner Autoscaler. To scale the target Cloud Spanner instance, the weakest predefined role is [`roles/spanner.admin`](https://cloud.google.com/spanner/docs/iam?hl=en#roles). To observe the CPU usage metric of the project of the Spanner instance, the weakest predefined role is [`roles/monitoring.viewer`](https://cloud.google.com/monitoring/access-control?hl=en#monitoring_2).\
  The custom role can be created with just the following permissions:
  - `spanner.instances.get`
  - `spanner.instances.update`
  - `monitoring.timeSeries.list`

  </details>
</ul>

</details>


## Development

Run `make help` for a list of useful targets. The installation basically has 3 steps:

```
## 1. Installation of CRD
$ make install

## 2. Deployment of the operator
$ make deploy

## 3. Creation of a CRD
$ kubectl apply -f config/samples
```

Test the operator with `make test`

> :warning: **Migration from `v0.1.5`:** Names of some resources (`Deployment`, `serviceAccount`,`Role` etc) have changed since version `0.1.5`. Thus, you must first uninstall the old version before installing the new version. To uninstall the old version:
> ```console
> $ git checkout v0.1.5
> $ kustomize build config/default | kubectl delete -f -
> ```
> Specifically, the kubernetes service account used for running the spanner-autoscaler has changed from `default` to `spanner-autoscaler-controller-manager`. Please keep this in mind. It is recommended to follow the below configuration steps and re-create any resources if needed.


## Contribution

See [CONTRIBUTING.md](.github/CONTRIBUTING.md).

## License

Spanner Autoscaler is released under the [Apache License 2.0](./LICENSE).

:warning: **NOTE:**

1. This project is currently in active development phase and there might be some backward incompatible changes in future versions.
1. Spanner Autoscaler watches `High Priority` CPU utilization only. It doesn't watch `Low Priority` CPU utilization and Rolling average 24 hour utilization.
1. It doesn't check [the storage size and the number of databases](https://cloud.google.com/spanner/quotas?hl=en#database_limits) as well. You must take care of these metrics by yourself.


<!-- badge links -->

[actions-workflow-test]: https://github.com/mercari/spanner-autoscaler/actions?query=workflow%3ATest
[actions-workflow-test-badge]: https://img.shields.io/github/workflow/status/mercari/spanner-autoscaler/Test?label=Test&style=for-the-badge&logo=github

[release]: https://github.com/mercari/spanner-autoscaler/releases
[release-badge]: https://img.shields.io/github/v/release/mercari/spanner-autoscaler?style=for-the-badge&logo=github

[license]: LICENSE
[license-badge]: https://img.shields.io/github/license/mercari/spanner-autoscaler?style=for-the-badge
