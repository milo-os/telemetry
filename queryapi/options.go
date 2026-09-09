// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/spf13/pflag"
	"k8s.io/apiserver/pkg/server/options"

	"go.datum.net/o11y/queryapi/internal/authz"
	"go.datum.net/o11y/queryapi/internal/storage"
)

// Options is queryapi's whole flag surface.
//
// It is built on the apiserver's own RecommendedOptions because queryapi is an
// aggregated API server: kube-aggregator will not proxy a request to it until
// it answers a discovery probe, it cannot believe the identity Milo forwards
// until the delegating authenticator has verified the front proxy's client
// certificate on the connection, and its authorization is Milo's to decide.
// All three come from the framework, and they have to be wired to each other
// the way the framework wires them.
type Options struct {
	// Recommended carries serving, authentication, authorization, auditing and
	// tracing -- the same options activity runs on, with the same flag names.
	//
	// Etcd and Admission are nil: queryapi stores nothing and admits nothing,
	// because its data lives in ClickHouse. CoreAPI is nil too; nothing here
	// reads the core API, and the SubjectAccessReview client the delegating
	// authorizer needs is built from --authorization-kubeconfig rather than
	// from that one.
	Recommended *options.RecommendedOptions

	// Query is what the handlers need.
	Query Config
}

// NewOptions returns Options with production-reasonable defaults.
func NewOptions() *Options {
	// The prefix and codec arguments only ever reach EtcdOptions, which is
	// discarded immediately below; queryapi persists nothing.
	recommended := options.NewRecommendedOptions("", Codecs.LegacyCodec())
	recommended.Etcd = nil
	recommended.Admission = nil
	recommended.CoreAPI = nil

	// Required: an operator cannot switch the listener off with
	// --secure-port=0, because a queryapi that serves nothing over TLS serves
	// nothing at all -- only a TLS connection can carry the front proxy's
	// client certificate.
	recommended.SecureServing.Required = true
	// 8443 matches the activity service, the reference for how a Milo backend
	// terminates TLS behind the aggregator.
	recommended.SecureServing.BindPort = 8443
	recommended.SecureServing.ServerCert.PairName = "queryapi"

	// Nothing here needs pprof, and leaving it on would also swap the
	// read-only /metrics handler for the one that resets the registry on
	// DELETE -- on a path --authorization-always-allow-paths leaves
	// unauthorized.
	recommended.Features.EnableProfiling = false
	// Priority and fairness needs a core API client and shared informers to
	// read FlowSchemas with, which is the only thing CoreAPI would have been
	// kept for. queryapi keeps the plain in-flight limiter instead; it has one
	// kind of request and no long-running ones.
	recommended.Features.EnablePriorityAndFairness = false

	// /metrics joins the probes outside authorization. The framework's default
	// covers only /healthz, /readyz and /livez; the scraper has no more of an
	// identity than the kubelet does. It cannot reach a query route: those are
	// resource requests by the time they are authorized, and the path
	// authorizer abstains on every one of those.
	recommended.Authorization.WithAlwaysAllowPaths("/metrics")

	return &Options{
		Recommended: recommended,
		Query:       DefaultConfig(),
	}
}

// AddFlags binds every option to fs.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// --secure-port, --tls-*, --cert-dir, --authentication-kubeconfig,
	// --requestheader-*, --authorization-kubeconfig,
	// --authorization-always-allow-paths, --audit-* and friends, with the same
	// names and meanings activity's deployment uses.
	o.Recommended.AddFlags(fs)

	fs.StringVar(&o.Query.Storage, "storage", o.Query.Storage, "storage backend: fake or clickhouse")
	fs.Float64Var(&o.Query.FakeRate, "fake-rate", o.Query.FakeRate,
		"synthetic log lines per second (fake backend)")
	fs.DurationVar(&o.Query.QueryTimeout, "query-timeout", o.Query.QueryTimeout,
		"maximum duration of a single storage query")
	fs.IntVar(&o.Query.DefaultLimit, "default-limit", o.Query.DefaultLimit, "default maximum log lines returned")
	fs.IntVar(&o.Query.MaxLimit, "max-limit", o.Query.MaxLimit, "hard ceiling on log lines returned")

	// There is deliberately no flag that lets a caller name its own project.
	// The project comes from the verified identity's iam.miloapis.com/parent-name
	// extra, and from nowhere else.
}

// Validate reports whether the options are usable.
func (o *Options) Validate() error {
	errs := o.Recommended.Validate()
	if err := o.Query.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := authz.ValidateAlwaysAllowPaths(o.Recommended.Authorization.AlwaysAllowPaths); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Config assembles the server configuration.
func (o *Options) Config(logger *slog.Logger, store storage.LogStore) (*ServerConfig, error) {
	// With no --tls-cert-file the apiserver convention is to self-sign into
	// --cert-dir. That is the local-development path only: in the cluster the
	// serving certificate comes from the cert-manager CSI driver
	// (config/queryapi/deployment.yaml) and the root filesystem is read-only,
	// so this branch cannot be taken there.
	if err := o.Recommended.SecureServing.MaybeDefaultWithSelfSignedCerts(
		"localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("generate a self-signed serving certificate: %w", err)
	}

	generic := NewGenericConfig()
	// Order matters and always has: ApplyTo hands the secure serving info to
	// the delegating authenticator, which is what puts the front proxy's CA
	// into the listener's client CA pool. Build them the other way round and
	// the client certificate check silently never fires.
	if err := o.Recommended.ApplyTo(generic); err != nil {
		return nil, fmt.Errorf("apply the apiserver options: %w", err)
	}
	if generic.Authentication.Authenticator == nil {
		// There is no reduced mode that reads X-Remote-* without verifying it:
		// that would be trusting whatever a client sent.
		return nil, fmt.Errorf("delegating authentication produced no authenticator")
	}

	return &ServerConfig{
		Generic: generic,
		Extra:   ExtraConfig{Logger: logger, Store: store, Query: o.Query},
	}, nil
}
