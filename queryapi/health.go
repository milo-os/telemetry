// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"context"
	"net/http"
	"time"

	"k8s.io/apiserver/pkg/server/healthz"
)

type pinger interface{ Ping(context.Context) error }

// storageReady is queryapi's only addition to the framework's health surface.
//
// The apiserver runtime serves /healthz, /livez and /readyz itself, so there
// is nothing to register a route for -- only a check to add. It goes on
// /readyz alone: liveness must not depend on the storage backend, or an
// outage that merely makes queries fail would restart the pod as well.
func storageReady(name string, store pinger, timeout time.Duration) healthz.HealthChecker {
	return healthz.NamedCheck(name, func(r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		return store.Ping(ctx)
	})
}
