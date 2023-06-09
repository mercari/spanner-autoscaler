# Development (when webhooks are enabled)

This doc explains how to setup a local environment for development and testing of the spanner-autoscaler and the spanner-autoscale-schedule controllers. Since there are conversion, validation and mutation webhooks enabled for some CRDs, we need to perform some additional steps to ensure that k8s can communicate with our webhook servers correctly.

The default approach is to:
1. make any desired chagnes to the code
1. build and deploy the docker image and the CRDs:
   ```console
   $ make kind-cluster-reset
   $ export IMG=mercari/spanner-autoscaler:local
   $ make docker-build kind-load-docker-image
   $ make install deploy
   $ kubectl apply -k config/samples
   ```
If there are any errors in the conversion or the validation of the sample CRs, then the `kubectl apply` command above, will fail with the corresponding errors.

While this approach is useful, it is very time consuming for testing minor changes in real time during development (because `make docker-build` step takes a very long time sometimes). Thus, during development, it is preferable to run the controller and the webhooks locally (since they are all part of the same binary), and forward any requests for these components from the k8s cluster to the locally running server.

This can be achieved in the following way:
1. Modify your `/etc/hosts` to add a DNS record for your LAN IP (by adding `192.168.<your-lan-ip> dummy.local` to the `/etc/hosts` file)
1. Deploy the CRDs and the other resources to the kind cluster:
   ```console
   $ make kind-cluster-reset
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
1. Continue with development and testing by running the local server with `make run` command. To test any new changes, make the desired changes, stop the controller with `Ctrl-C` and then run `make run` again.

This will deploy the CRDs and other resources to the cluster, but will forward any controller or webhook related requests from k8s cluster to our locally running controller.

### Things to take care before sending a PR
- Run `make manifests docs` if you made any changes to any of the CRD defintions (related files: `api/*/*_types.go`)
