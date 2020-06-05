// +build tools

package tools

import (
	_ "github.com/GoogleContainerTools/kpt"
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
	_ "sigs.k8s.io/kind"
)
