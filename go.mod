module github.com/mercari/spanner-autoscaler

go 1.14

require (
	cloud.google.com/go v0.57.0
	cloud.google.com/go/spanner v1.6.0
	github.com/GoogleContainerTools/kpt v0.26.0
	github.com/go-logr/logr v0.1.0
	github.com/go-logr/zapr v0.1.1
	github.com/golang/protobuf v1.4.2
	github.com/google/go-cmp v0.4.1
	github.com/google/uuid v1.1.1
	go.uber.org/zap v1.15.0
	golang.org/x/oauth2 v0.0.0-20200107190931-bf48bf16ab8d
	golang.org/x/sync v0.0.0-20200317015054-43a5402ce75a
	google.golang.org/api v0.26.0
	google.golang.org/genproto v0.0.0-20200526151428-9bb895338b15
	k8s.io/api v0.18.3
	k8s.io/apiextensions-apiserver v0.18.2
	k8s.io/apimachinery v0.18.3
	k8s.io/client-go v0.18.3
	sigs.k8s.io/controller-runtime v0.6.0
	sigs.k8s.io/controller-tools v0.3.0
	sigs.k8s.io/kind v0.7.0
)
