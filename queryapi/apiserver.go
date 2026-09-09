// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	genericapiserver "k8s.io/apiserver/pkg/server"
	basecompatibility "k8s.io/component-base/compatibility"

	"go.datum.net/o11y/queryapi/internal/authz"
	"go.datum.net/o11y/queryapi/internal/miloauth"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// kubeVersion is the Kubernetes version this server reports as its effective
// version. It tracks the k8s.io/* modules in go.mod, as activity's does.
const kubeVersion = "1.34"

// ExtraConfig is everything the generic apiserver knows nothing about.
type ExtraConfig struct {
	Logger *slog.Logger
	Store  storage.LogStore
	Query  Config
}

// ServerConfig combines the generic apiserver configuration with queryapi's.
type ServerConfig struct {
	Generic *genericapiserver.RecommendedConfig
	Extra   ExtraConfig
}

type completedServerConfig struct {
	generic genericapiserver.CompletedConfig
	extra   *ExtraConfig
	router  *router
}

// CompletedServerConfig cannot be built except by Complete, so an incomplete
// configuration cannot be started.
type CompletedServerConfig struct {
	*completedServerConfig
}

// NewGenericConfig returns the generic apiserver configuration queryapi runs
// on, before authentication, authorization and serving options are applied to
// it.
//
// Profiling is off: nothing here needs pprof, and leaving it on would also
// swap the read-only /metrics handler for the one that resets the registry on
// DELETE -- on a path that --authorization-always-allow-paths leaves
// unauthorized. Options.Config keeps it off through the flag that would
// otherwise turn it back on.
//
// There is no etcd and no admission anywhere in this file: queryapi stores
// nothing and admits nothing, because all of its data is in ClickHouse.
func NewGenericConfig() *genericapiserver.RecommendedConfig {
	cfg := genericapiserver.NewRecommendedConfig(Codecs)
	cfg.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString(kubeVersion, "", "")
	cfg.EnableProfiling = false
	return cfg
}

// Complete fills in what New needs and cannot derive later.
//
// It returns an error, unlike the framework's Complete, because the route
// table is built here: a route whose permission is not in the vocabulary has
// to stop the process, and there is no later point at which that is still a
// startup failure rather than a caller's 403.
func (c *ServerConfig) Complete() (CompletedServerConfig, error) {
	if c.Extra.Store == nil {
		return CompletedServerConfig{}, fmt.Errorf("a storage backend is required")
	}
	if c.Extra.Logger == nil {
		return CompletedServerConfig{}, fmt.Errorf("a logger is required")
	}

	router, err := newRouter(c.Extra.Logger, c.Extra.Store, c.Extra.Query)
	if err != nil {
		return CompletedServerConfig{}, err
	}

	// Every server is built through here, so this is the one place the
	// authorization chain is assembled -- and the one place it could be left
	// out. Guard refuses a nil authorizer rather than letting the framework
	// turn WithAuthorization into a pass-through.
	guarded, err := authz.Guard(c.Generic.Authorization.Authorizer)
	if err != nil {
		return CompletedServerConfig{}, err
	}
	c.Generic.Authorization.Authorizer = guarded

	// The route table describes what is served, so it also describes what is
	// authorized. Setting the resolver here, before Complete installs the
	// stock one, is what puts the Loki- and Prometheus-shaped paths into the
	// framework's own WithAuthorization filter instead of a parallel
	// middleware of our own.
	c.Generic.RequestInfoResolver = router.requestInfoResolver(
		genericapiserver.NewRequestInfoResolver(&c.Generic.Config))

	// withCanonicalPaths has to run before WithRequestInfo, which means
	// wrapping the whole chain rather than joining it.
	c.Generic.BuildHandlerChainFunc = func(apiHandler http.Handler, cfg *genericapiserver.Config) http.Handler {
		return withCanonicalPaths(genericapiserver.DefaultBuildHandlerChain(apiHandler, cfg))
	}

	return CompletedServerConfig{&completedServerConfig{
		generic: c.Generic.Complete(),
		extra:   &c.Extra,
		router:  router,
	}}, nil
}

// Server is the query API, served by the Kubernetes apiserver runtime.
type Server struct {
	GenericAPIServer *genericapiserver.GenericAPIServer

	logger *slog.Logger
	store  storage.LogStore
}

// New builds the server and installs everything queryapi adds to the generic
// one: the discovery document for its group and version, the telemetry routes,
// and the storage readiness check.
func (c CompletedServerConfig) New() (*Server, error) {
	generic, err := c.generic.New("queryapi", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	s := &Server{GenericAPIServer: generic, logger: c.extra.Logger, store: c.extra.Store}
	s.installDiscovery()
	s.installRoutes(c.extra.Logger, c.router)

	// The framework serves /readyz; this is the check that used to be a
	// hand-registered route. /healthz and /livez stay independent of storage,
	// or an outage that only makes queries fail would also restart the pod.
	if err := generic.AddReadyzChecks(storageReady(c.extra.Store, c.extra.Query.QueryTimeout)); err != nil {
		return nil, fmt.Errorf("register the storage readiness check: %w", err)
	}

	return s, nil
}

// installDiscovery serves /apis/o11y.miloapis.com and
// /apis/o11y.miloapis.com/v1alpha1.
//
// This is what makes the APIService usable at all. kube-aggregator's
// availability controller probes GET /apis/{group}/{version} on the backend
// and marks the APIService unavailable on anything that is not a 2xx or 3xx;
// an unavailable APIService is never proxied to, so without this document not
// one query would arrive.
//
// The resource list is empty, and honestly so: queryapi serves no Kubernetes
// resources. `logs` and `metrics` name permissions, not REST endpoints, and
// listing them here would tell kubectl about collections that do not exist.
func (s *Server) installDiscovery() {
	apiGroup := metav1.APIGroup{
		Name: GroupVersion.Group,
		Versions: []metav1.GroupVersionForDiscovery{{
			GroupVersion: GroupVersion.String(),
			Version:      GroupVersion.Version,
		}},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: GroupVersion.String(),
			Version:      GroupVersion.Version,
		},
	}

	// The two discovery managers behind /apis, so that a client listing groups
	// -- in either the legacy or the aggregated format -- sees this one.
	s.GenericAPIServer.DiscoveryGroupManager.AddGroup(apiGroup)
	s.GenericAPIServer.AggregatedDiscoveryGroupManager.AddGroupVersion(
		GroupVersion.Group,
		apidiscoveryv2.APIVersionDiscovery{
			Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
			Version:   GroupVersion.Version,
		},
	)

	mux := s.GenericAPIServer.Handler.NonGoRestfulMux
	mux.Handle("/apis/"+GroupVersion.Group, discovery.NewAPIGroupHandler(Codecs, apiGroup))
	mux.Handle(APIPrefix, discovery.NewAPIVersionHandler(Codecs, GroupVersion,
		// An empty slice rather than nil, so the document says "resources": []
		// rather than "resources": null.
		discovery.APIResourceListerFunc(func() []metav1.APIResource { return []metav1.APIResource{} })))
}

// installRoutes hangs the tenant mux off the aggregator's path.
//
// It is registered as a prefix so that every path under the group and version
// lands here, including the ones no route serves: those are denied before they
// arrive, and answered 404 if an operator ever allows them. Nothing under this
// prefix reaches the generic server's own handlers.
func (s *Server) installRoutes(logger *slog.Logger, r *router) {
	s.GenericAPIServer.Handler.NonGoRestfulMux.HandlePrefix(APIPrefix+"/",
		miloauth.Middleware(logger, r.mux))
}

// Run serves until ctx is cancelled, then drains in-flight queries before
// closing the storage backend.
func (s *Server) Run(ctx context.Context) error {
	defer func() {
		closer, ok := s.store.(io.Closer)
		if !ok {
			return
		}
		if err := closer.Close(); err != nil {
			s.logger.Error("close storage", "error", err)
		}
	}()
	return s.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
