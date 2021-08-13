module github.com/mercari/spanner-autoscaler

go 1.14

require (
	cloud.google.com/go v0.87.0
	cloud.google.com/go/spanner v1.6.0
	github.com/GoogleContainerTools/kpt v0.26.0
	github.com/go-logr/logr v0.1.0
	github.com/go-logr/zapr v0.1.1
	github.com/golang/protobuf v1.5.2
	github.com/google/go-cmp v0.5.6
	github.com/google/uuid v1.1.2
	go.uber.org/zap v1.15.0
	golang.org/x/oauth2 v0.0.0-20210628180205-a41e5a781914
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	google.golang.org/api v0.51.0
	google.golang.org/genproto v0.0.0-20210716133855-ce7ef5c701ea
	k8s.io/api v0.18.3
	k8s.io/apiextensions-apiserver v0.18.2
	k8s.io/apimachinery v0.18.3
	k8s.io/client-go v0.18.3
	sigs.k8s.io/controller-runtime v0.6.0
	sigs.k8s.io/controller-tools v0.3.0
	sigs.k8s.io/kind v0.7.0
)
