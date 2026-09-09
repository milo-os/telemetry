// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v, want nil", err)
	}

	cases := map[string]Config{
		"zero default":      {DefaultLimit: 0, MaxLimit: 5000, QueryTimeout: time.Second},
		"negative default":  {DefaultLimit: -1, MaxLimit: 5000, QueryTimeout: time.Second},
		"zero max":          {DefaultLimit: 100, MaxLimit: 0, QueryTimeout: time.Second},
		"default above max": {DefaultLimit: 10000, MaxLimit: 100, QueryTimeout: time.Second},
		"zero timeout":      {DefaultLimit: 100, MaxLimit: 5000},
	}
	for name, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

// TestDefaultOptionsServeNothingFromEtcd pins the shape of the server this
// service is: an aggregated apiserver with no storage of its own and no
// admission chain. Both would otherwise arrive with RecommendedOptions and
// bring flags -- and a required etcd -- with them.
func TestDefaultOptionsServeNothingFromEtcd(t *testing.T) {
	o := NewOptions()
	if o.Recommended.Etcd != nil {
		t.Error("Etcd options are set; queryapi's data is in ClickHouse")
	}
	if o.Recommended.Admission != nil {
		t.Error("admission options are set; queryapi admits nothing")
	}

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	o.AddFlags(fs)
	for _, name := range []string{"etcd-servers", "enable-admission-plugins"} {
		if fs.Lookup(name) != nil {
			t.Errorf("--%s is registered", name)
		}
	}
	// The flags the deployment sets, and the ones the identity chain depends on.
	for _, name := range []string{
		"secure-port", "tls-cert-file", "tls-private-key-file",
		"authentication-kubeconfig", "requestheader-client-ca-file",
		"authorization-kubeconfig", "authorization-always-allow-paths",
		"storage", "query-timeout",
	} {
		if fs.Lookup(name) == nil {
			t.Errorf("--%s is not registered", name)
		}
	}
	// There has never been a flag by which a caller names its own project, and
	// there must not be one now.
	fs.VisitAll(func(f *pflag.Flag) {
		if strings.Contains(f.Name, "project") {
			t.Errorf("--%s exists: nothing but a verified identity may name the project", f.Name)
		}
	})
}

// TestDefaultOptionsLeaveProbesUnauthorized pins requirement of every probe:
// the kubelet presents no credentials, and the scraper has no more of an
// identity than it does.
func TestDefaultOptionsLeaveProbesUnauthorized(t *testing.T) {
	got := map[string]bool{}
	for _, p := range NewOptions().Recommended.Authorization.AlwaysAllowPaths {
		got[p] = true
	}
	for _, want := range []string{"/healthz", "/readyz", "/livez", "/metrics"} {
		if !got[want] {
			t.Errorf("%s is not in --authorization-always-allow-paths by default", want)
		}
	}
}

// TestOptionsRejectAlwaysAllowingTheAPI pins that Validate refuses a value of
// --authorization-always-allow-paths that reaches into this service's own
// group, rather than letting an operator believe it worked.
func TestOptionsRejectAlwaysAllowingTheAPI(t *testing.T) {
	o := NewOptions()
	o.Recommended.Authorization.AlwaysAllowPaths = append(
		o.Recommended.Authorization.AlwaysAllowPaths, "/apis/o11y.miloapis.com/*")
	if err := o.Validate(); err == nil {
		t.Fatal("Validate() = nil, want a refusal")
	}
}
