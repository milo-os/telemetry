// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"go.datum.net/o11y/queryapi/internal/authz"
)

// GroupVersion is the group and version kube-aggregator registers queryapi at,
// and the one its discovery document and every reviewed permission name.
var GroupVersion = schema.GroupVersion{Group: authz.APIGroup, Version: authz.APIVersion}

var (
	// Scheme carries only the API machinery's own meta types. queryapi serves
	// no Kubernetes resources -- its payloads are Loki's and Prometheus's JSON,
	// and its data lives in ClickHouse rather than etcd -- so the only objects
	// that ever need encoding here are the discovery documents and the Status
	// the framework's filters write on a 401, 403 or 500.
	Scheme = runtime.NewScheme()

	// Codecs is the negotiated serializer the generic apiserver is built with.
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	Scheme.AddUnversionedTypes(schema.GroupVersion{Group: "", Version: "v1"},
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
}
