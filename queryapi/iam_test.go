// SPDX-License-Identifier: AGPL-3.0-only

package queryapi

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/yaml"

	"go.datum.net/o11y/queryapi/internal/authz"
	"go.datum.net/o11y/queryapi/internal/storage/fake"
)

const iamDir = "../config/operator/iam"

type protectedResource struct {
	Spec struct {
		ServiceRef struct {
			Name string `json:"name"`
		} `json:"serviceRef"`
		Plural       string   `json:"plural"`
		Permissions  []string `json:"permissions"`
		Subresources []struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		} `json:"subresources"`
	} `json:"spec"`
}

type role struct {
	Spec struct {
		IncludedPermissions []string `json:"includedPermissions"`
	} `json:"spec"`
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func declaredPermissions(t *testing.T) sets.Set[string] {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(iamDir, "protected-resources", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	declared := sets.New[string]()
	for _, f := range files {
		var pr protectedResource
		readYAML(t, f, &pr)
		if pr.Spec.ServiceRef.Name != authz.APIGroup {
			continue
		}
		base := pr.Spec.ServiceRef.Name + "/" + pr.Spec.Plural
		for _, verb := range pr.Spec.Permissions {
			declared.Insert(base + "." + verb)
		}
		for _, sub := range pr.Spec.Subresources {
			for _, verb := range sub.Permissions {
				declared.Insert(base + "/" + sub.Name + "." + verb)
			}
		}
	}
	if declared.Len() == 0 {
		t.Fatalf("no %s permissions declared under %s", authz.APIGroup, iamDir)
	}
	return declared
}

// Each route's aggregator permission must be declared and granted.
func TestAggregatorPermissionsAreDeclaredAndGranted(t *testing.T) {
	declared := declaredPermissions(t)

	var viewer role
	readYAML(t, filepath.Join(iamDir, "roles", "telemetry-viewer.yaml"), &viewer)
	granted := sets.New(viewer.Spec.IncludedPermissions...)

	router, err := newRouter(slog.New(slog.DiscardHandler), fake.New(1), DefaultConfig())
	if err != nil {
		t.Fatalf("newRouter: %v", err)
	}
	resolver := &request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}

	for pattern := range router.permissions {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("pattern %q has no method", pattern)
		}
		path = strings.ReplaceAll(path, "{name}", "job")
		t.Run(pattern, func(t *testing.T) {
			info, err := resolver.NewRequestInfo(httptest.NewRequest(method, path, nil))
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if info.APIGroup != authz.APIGroup || info.Subresource == "" {
				t.Fatalf("resolved %+v, want a subresource under %s", info, authz.APIGroup)
			}
			base := info.APIGroup + "/" + info.Resource
			want := []string{base + "/" + info.Subresource + "." + info.Verb}
			if method == "GET" {
				want = append(want, base+"."+info.Verb)
			}
			for _, permission := range want {
				if !declared.Has(permission) {
					t.Errorf("aggregator asks for %s, which no ProtectedResource declares", permission)
				}
				if !granted.Has(permission) {
					t.Errorf("aggregator asks for %s, which the viewer role does not grant", permission)
				}
			}
		})
	}
}

func TestViewerRoleGrantsOnlyDeclaredPermissions(t *testing.T) {
	declared := declaredPermissions(t)

	var viewer role
	readYAML(t, filepath.Join(iamDir, "roles", "telemetry-viewer.yaml"), &viewer)
	for _, permission := range viewer.Spec.IncludedPermissions {
		if strings.HasPrefix(permission, authz.APIGroup+"/") && !declared.Has(permission) {
			t.Errorf("viewer role grants %s, which no ProtectedResource declares", permission)
		}
	}
}
