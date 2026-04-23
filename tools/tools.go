//go:build tools

package tools

import (
	_ "github.com/elastic/crd-ref-docs"
	_ "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
	_ "sigs.k8s.io/kind"
)
