// SPDX-License-Identifier: AGPL-3.0-only

// Package miloauth resolves the project a request is scoped to from the
// identity the apiserver runtime authenticated, and carries it to the storage
// layer.
package miloauth

import "context"

type ctxKey struct{}

// WithProject returns ctx scoped to a project.
func WithProject(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// ProjectID returns the project on ctx, if any.
func ProjectID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}
