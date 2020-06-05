package logging

import (
	"context"

	"github.com/go-logr/logr"
)

type ctxKey struct{}

func WithContext(ctx context.Context, log logr.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

func FromContext(ctx context.Context) logr.Logger {
	return ctx.Value(ctxKey{}).(logr.Logger)
}
