# Webhook development

This doc explains how to setup a local environment for development and testing of conversion, validation or mutation webhooks. This approach can also be used for development and testing of the k8s controller too.

The default approach is to:
1. make any desired chagnes to the webhook code in `api/<version>/<crd>_webhook.go`
1. build and deploy the docker image and the CRDs:
   ```console
   $ make kind-cluster-reset
   $ kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.6.1/cert-manager.yaml
   $ export IMG=mercari/spanner-autoscaler:local
   $ make docker-build
   $ kind load docker-image --name spanner-autoscaler $IMG
   $ make deploy
   $ kubectl apply -f config/samples
   ```
If there are any errors in the conversion or the validation, then the `kubectl apply` command above, will fail with the corresponding errors.

While this approach is useful, it is very time consuming for testing minor changes in real time during development. Thus, during development, it is preferable to run the controller and the webhooks (since they are all part of the same binary) locally, and forward any requests for these components from the k8s cluster to the locally running server.

This can be achieved in the following way:
1. Modify the `/etc/hosts` to add a DNS record for your LAN IP (by adding `192.168.<your-lan-ip> dummy.local` to the `/etc/hosts` file)
1. Deploy the CRDs and the other resources to the kind cluster:
   ```console
   $ make kind-cluster-reset
   $ kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.6.1/cert-manager.yaml
   $ make deploy-dev
   ```
1. Save the TLS certificates which the local server can use:
   ```console
   $ mkdir bin/dummytls
   $ kubectl get secret -n spanner-autoscaler webhook-server-cert -o yaml | yq e '.data."tls.crt"' - | base64 -d > bin/dummytls/tls.crt
   $ kubectl get secret -n spanner-autoscaler webhook-server-cert -o yaml | yq e '.data."tls.key"' - | base64 -d > bin/dummytls/tls.key
   ```
   Make changes in `main.go` to use these local certificates:
   ```diff
    mgr, err := ctrlmanager.New(cfg, ctrlmanager.Options{
      Scheme:                 scheme,
      LeaderElection:         *enableLeaderElection,
      LeaderElectionID:       leaderElectionID,
      MetricsBindAddress:     *metricsAddr,
      HealthProbeBindAddress: *probeAddr,
      ReadinessEndpointName:  readyzEndpoint,
      LivenessEndpointName:   healthzEndpoint,
    
   +  // TODO: remove this when `v1beta1` is stable and tested
   +  // Only for development
   +  CertDir: "./bin/dummytls",
    })
   ```
1. Continue with development and testing by running the local server with `make run` command. To test any new changes, make the desired changes, stop the controller with `Ctrl-C` and the run `make run` again.

This will deploy the CRDs and other resources to the cluster, but will forward any controller or webhook related requests from k8s cluster to our locally running controller.
