// SPDX-License-Identifier: AGPL-3.0-only

// Package authz holds the permission vocabulary queryapi's routes are
// reviewed against, and the guard that keeps anything outside that vocabulary
// from being reviewed at all. The SubjectAccessReview itself is the apiserver
// framework's, addressed at Milo.
package authz

import (
	"fmt"
	"strings"

	"k8s.io/apiserver/pkg/authorization/authorizer"
)

// APIGroup is the Milo service the query surface is registered under, and
// APIVersion the version its routes are served at. Milo permissions are
// {service}/{resource}.{action}, so APIGroup plus a Permission spells
// o11y.miloapis.com/logs.query.
//
// They are also the group and version kube-aggregator registers queryapi at
// (config/queryapi-api-registration/apiservice.yaml), which is why the served
// paths and the reviewed attributes can be derived from one another at all.
const (
	APIGroup   = "o11y.miloapis.com"
	APIVersion = "v1alpha1"
)

// Permission is the resource and action a route requires.
type Permission struct {
	Resource string
	Verb     string
}

// Permissions is the whole vocabulary this service reviews against. It is a
// closed set, and Guard reviews nothing outside it: attributes naming this
// service's group with a resource and verb not listed here are denied without
// a review rather than sent to Milo as something it might happen to allow.
//
// The metadata endpoints take their own actions rather than sharing one read
// permission: /label/{name}/values returns pod names, hostnames and customer
// identifiers, which reading log lines does not necessarily imply, and that
// boundary cannot be retrofitted once a single action covers both.
//
// The metrics handlers return 501 today. Gating a stub costs nothing and keeps
// the endpoint from shipping unguarded later.
var Permissions = map[Permission]struct{}{
	{Resource: "logs", Verb: "query"}:        {},
	{Resource: "logs", Verb: "getLabels"}:    {},
	{Resource: "logs", Verb: "getSeries"}:    {},
	{Resource: "metrics", Verb: "query"}:     {},
	{Resource: "metrics", Verb: "getLabels"}: {},
	{Resource: "metrics", Verb: "getSeries"}: {},
}

// Known reports whether p is in the vocabulary. Route tables use it at
// construction time so a typo is a startup failure rather than a caller's 403.
func Known(p Permission) bool {
	_, ok := Permissions[p]
	return ok
}

// permissionFor returns the permission attrs asks for, if attrs names one at
// all. Everything is checked: a request that is not a resource request, or
// that names another group, version, resource or verb, has no permission here
// and is denied.
func permissionFor(attrs authorizer.Attributes) (Permission, bool) {
	if attrs == nil || !attrs.IsResourceRequest() {
		return Permission{}, false
	}
	if attrs.GetAPIGroup() != APIGroup || attrs.GetAPIVersion() != APIVersion {
		return Permission{}, false
	}
	// A subresource, a name or a namespace would mean the attributes came from
	// somewhere other than this service's route table, which is the only thing
	// allowed to describe what it serves.
	if attrs.GetSubresource() != "" || attrs.GetName() != "" || attrs.GetNamespace() != "" {
		return Permission{}, false
	}
	p := Permission{Resource: attrs.GetResource(), Verb: attrs.GetVerb()}
	if !Known(p) {
		return Permission{}, false
	}
	return p, true
}

// ValidateAlwaysAllowPaths rejects a value of --authorization-always-allow-paths
// that would reach into this service's own API group.
//
// Health and metrics are the point of that flag; the query surface is not. A
// telemetry route is a resource request by the time it is authorized and
// path.NewAuthorizer abstains on those, so such a value could not actually
// open one -- but an operator who wrote it believes otherwise, and should find
// out at startup rather than from an audit.
func ValidateAlwaysAllowPaths(paths []string) error {
	const apiPrefix = "/apis/" + APIGroup
	for _, p := range paths {
		normalized := "/" + strings.TrimPrefix(p, "/")
		if strings.HasPrefix(normalized, apiPrefix) ||
			strings.HasPrefix(apiPrefix, strings.TrimSuffix(normalized, "*")) {
			return fmt.Errorf("authorization-always-allow-paths may not cover %s: %q", apiPrefix, p)
		}
	}
	return nil
}
