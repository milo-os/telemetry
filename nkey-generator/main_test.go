// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func TestGenerateNKey(t *testing.T) {
	kp, err := generateNKey()
	if err != nil {
		t.Fatalf("generateNKey: %v", err)
	}

	// Seed must be a valid base32-encoded NKey user seed ("SU" prefix).
	if !strings.HasPrefix(string(kp.Seed), "SU") {
		t.Fatalf("seed does not start with SU prefix: %q", kp.Seed)
	}

	decoded, err := nkeys.FromSeed(kp.Seed)
	if err != nil {
		t.Fatalf("FromSeed: %v", err)
	}
	defer decoded.Wipe()

	pubFromSeed, err := decoded.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if pubFromSeed != kp.Public {
		t.Fatalf("public key mismatch: fromSeed=%q pair=%q", pubFromSeed, kp.Public)
	}
}

func TestNkeyUserAllowlist(t *testing.T) {
	u := nkeyUser("us-east-1", "UD5ULF5HDXSAB42CKHYDGDQ5E53AJNSXYW72MEYMHNVGAQKNRZS55H5N")

	if u["nkey"] != "UD5ULF5HDXSAB42CKHYDGDQ5E53AJNSXYW72MEYMHNVGAQKNRZS55H5N" {
		t.Fatalf("nkey not set")
	}
	if _, hasUser := u["user"]; hasUser {
		t.Fatalf("nkey user must not carry a user field")
	}

	publish := mustStrSlice(t, u, "publish")
	expected := []string{
		"o11y.logs.us-east-1.*",
		"o11y.metrics.us-east-1.*",
		"o11y.traces.us-east-1.*",
	}
	if len(publish) != len(expected) {
		t.Fatalf("publish allow len=%d want %d: %v", len(publish), len(expected), publish)
	}
	for i, e := range expected {
		if publish[i] != e {
			t.Fatalf("publish[%d]=%q want %q", i, publish[i], e)
		}
	}

	// The whole point: a leaf must never get JetStream API access. $JS.API.>
	// lets a holder create a consumer filtered to another PoP's subjects and
	// pull it via the allowed _INBOX.> subscribe -- that's a full bypass of
	// the subject allowlist above, not just an admin-plane nicety.
	for _, allow := range publish {
		if allow == ">" ||
			strings.HasPrefix(allow, "o11y.logs.*") ||
			strings.HasPrefix(allow, "$JS.") {
			t.Fatalf("leaf allowlist widened beyond its own PoP: %q", allow)
		}
	}

	sub := mustStrSlice(t, u, "subscribe")
	if len(sub) != 1 || sub[0] != "_INBOX.>" {
		t.Fatalf("subscribe allow = %v, want [_INBOX.>]", sub)
	}
}

func TestValidClusterName(t *testing.T) {
	valid := []string{"us-east-1", "edge-pop-42", "a", "a1"}
	for _, name := range valid {
		if !validClusterName(name) {
			t.Errorf("validClusterName(%q) = false, want true", name)
		}
	}

	// Subject-metacharacters or multi-token names must never reach
	// nkeyUser's string interpolation, even though Karmada Cluster names are
	// DNS-1123 subdomains today (which permit '.') -- a single dot silently
	// changes the subject's token count from the fixed 3-token shape the
	// rest of the system assumes.
	invalid := []string{"a.b", "a>", "a*", "a b", "", strings.Repeat("a", 254)}
	for _, name := range invalid {
		if validClusterName(name) {
			t.Errorf("validClusterName(%q) = true, want false", name)
		}
	}
}

func TestDroppedNkeysDetection(t *testing.T) {
	prev := []map[string]any{
		nkeyUser("us-east-1", "UD5ULF5HDXSAB42CKHYDGDQ5E53AJNSXYW72MEYMHNVGAQKNRZS55H5N"),
		nkeyUser("us-west-1", "UAAAA5HDXSAB42CKHYDGDQ5E53AJNSXYW72MEYMHNVGAQKNRZS55H5N"),
	}
	// us-west-1's nkey user vanished from the new set -- must be caught, but
	// only while its Secret still exists.
	next := []map[string]any{
		nkeyUser("us-east-1", "UD5ULF5HDXSAB42CKHYDGDQ5E53AJNSXYW72MEYMHNVGAQKNRZS55H5N"),
	}
	cfg := config{namespace: "o11y-system", secretPrefix: "nats-leaf-nkey"}

	g := &generator{cfg: cfg, local: k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nats-leaf-nkey-us-west-1", Namespace: "o11y-system"},
	})}
	dropped, err := g.droppedNkeys(context.Background(), prev, next)
	if err != nil {
		t.Fatalf("droppedNkeys: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "us-west-1" {
		t.Fatalf("droppedNkeys = %v, want [us-west-1] (its Secret still exists)", dropped)
	}

	// Once the operator deletes us-west-1's Secret -- the explicit signal
	// that this is an intentional decommission -- it must stop being
	// reported, or the safety check could never be satisfied: prev is read
	// back from the very ConfigMap it guards.
	g2 := &generator{cfg: cfg, local: k8sfake.NewSimpleClientset()}
	dropped2, err := g2.droppedNkeys(context.Background(), prev, next)
	if err != nil {
		t.Fatalf("droppedNkeys: %v", err)
	}
	if len(dropped2) != 0 {
		t.Fatalf("droppedNkeys = %v, want none once the Secret is gone", dropped2)
	}

	// A rotated key for the same cluster is also a drop of the old key --
	// that's still correct: the old key really is no longer authorized.
	dropped3, err := g.droppedNkeys(context.Background(), prev, prev)
	if err != nil {
		t.Fatalf("droppedNkeys: %v", err)
	}
	if len(dropped3) != 0 {
		t.Fatalf("identical sets must report no drops")
	}
}

func TestConfigFromEnvSecretStoreDefaults(t *testing.T) {
	t.Setenv("KARMADA_KUBECONFIG", "/tmp/kubeconfig")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.secretStoreName != "gcp-secret-store" {
		t.Fatalf("secretStoreName = %q, want gcp-secret-store", cfg.secretStoreName)
	}
	if cfg.secretStoreKind != "ClusterSecretStore" {
		t.Fatalf("secretStoreKind = %q, want ClusterSecretStore", cfg.secretStoreKind)
	}
}

func TestStaticUsers(t *testing.T) {
	users := staticUsers()
	if len(users) != 2 {
		t.Fatalf("expected 2 static users, got %d", len(users))
	}

	// NACK has full access.
	nack := users[0]
	if nack["user"] != "CN=nack.nats.client" {
		t.Fatalf("nack user = %v", nack["user"])
	}
	if got := mustStrSlice(t, nack, "publish"); len(got) != 1 || got[0] != ">" {
		t.Fatalf("nack publish = %v, want [>]", got)
	}

	// Sink is restricted to JetStream + o11y subscribe.
	sink := users[1]
	if sink["user"] != "CN=o11y-sink-nats-client" {
		t.Fatalf("sink user = %v", sink["user"])
	}

	// A pull consumer acks by publishing to $JS.ACK.>; without it the sink
	// pulls and never acks, and nothing ever leaves the stream.
	sinkPub := mustStrSlice(t, sink, "publish")
	var hasAck bool
	for _, allow := range sinkPub {
		if allow == "$JS.ACK.>" {
			hasAck = true
		}
	}
	if !hasAck {
		t.Fatalf("sink publish = %v, want it to include $JS.ACK.>", sinkPub)
	}
}

func TestRenderedValuesRoundTrip(t *testing.T) {
	users := append(staticUsers(), nkeyUser("us-east-1", "UD5ULF5HDXSAB42CKHYDGDQ5E53AJNSXYW72MEYMHNVGAQKNRZS55H5N"))

	// Exercise the same marshal path writeConfigMap uses by rendering the
	// document to bytes and asserting key structural markers survive.
	doc := map[string]any{
		"config": map[string]any{
			"merge": map[string]any{
				"accounts": map[string]any{
					"O11Y": map[string]any{
						"jetstream": "enabled",
						"users":     users,
					},
				},
			},
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, want := range []string{
		"O11Y:",
		"jetstream: enabled",
		"nkey: UD5ULF5",
		"o11y.logs.us-east-1.*",
		"CN=nack.nats.client",
		"CN=o11y-sink-nats-client",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("rendered values missing %q:\n%s", want, out)
		}
	}
}

func mustStrSlice(t *testing.T, m map[string]any, dir string) []string {
	t.Helper()
	perms, ok := m["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("no permissions map")
	}
	d, ok := perms[dir].(map[string]any)
	if !ok {
		t.Fatalf("no %s map", dir)
	}
	allow, ok := d["allow"].([]any)
	if !ok {
		t.Fatalf("no allow slice")
	}
	out := make([]string, 0, len(allow))
	for _, v := range allow {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("allow item not a string")
		}
		out = append(out, s)
	}
	return out
}

// updateResourceVersion runs fn against a fake dynamic client seeded with one
// existing object at the given GVR/resourceVersion, and returns the
// resourceVersion carried by the Update call fn triggers ("" if none was
// issued). The fake ObjectTracker itself does not enforce resourceVersion
// like a real API server does, so this inspects the outgoing Update object
// directly rather than relying on the call succeeding or failing.
func updateResourceVersion(t *testing.T, gvr schema.GroupVersionResource, listKind string, existing *unstructured.Unstructured, fn func(dyn *dynamicfake.FakeDynamicClient) error) string {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvr: listKind,
	}, existing)

	var gotRV string
	dyn.PrependReactor("update", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		gotRV = obj.GetResourceVersion()
		return false, nil, nil // let the fake tracker still handle it
	})

	if err := fn(dyn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return gotRV
}

// TestEnsurePushSecretUpdatesExisting guards against a regression where
// ensurePushSecret discarded its Get result and issued an Update with no
// resourceVersion, which a real API server rejects for every existing object.
func TestEnsurePushSecretUpdatesExisting(t *testing.T) {
	existing := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1alpha1",
		"kind":       "PushSecret",
		"metadata": map[string]any{
			"name":            "nats-leaf-nkey-us-east-1",
			"namespace":       "o11y-system",
			"resourceVersion": "42",
		},
	}}

	gotRV := updateResourceVersion(t, pushSecretsGVR, "PushSecretList", existing, func(dyn *dynamicfake.FakeDynamicClient) error {
		g := &generator{dyn: dyn, cfg: config{
			namespace:       "o11y-system",
			secretStoreName: "gcp-secret-store",
			secretStoreKind: "ClusterSecretStore",
		}}
		return g.ensurePushSecret(context.Background(), "us-east-1", "nats-leaf-nkey-us-east-1")
	})

	if gotRV != "42" {
		t.Fatalf("Update carried resourceVersion %q, want %q (the fetched object's)", gotRV, "42")
	}
}

// TestWriteConfigMapUpdatesExisting guards against the same missing-
// resourceVersion regression in writeConfigMap's update path.
func TestWriteConfigMapUpdatesExisting(t *testing.T) {
	existing := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            "nats-o11y-authorized-leafs",
			"namespace":       "o11y-system",
			"resourceVersion": "42",
		},
	}}

	users := append(staticUsers(), nkeyUser("us-east-1", "UD5ULF5HDXSAB42CKHYDGDQ5E53AJNSXYW72MEYMHNVGAQKNRZS55H5N"))
	gotRV := updateResourceVersion(t, configMapsGVR, "ConfigMapList", existing, func(dyn *dynamicfake.FakeDynamicClient) error {
		g := &generator{dyn: dyn, cfg: config{
			namespace:     "o11y-system",
			configMapName: "nats-o11y-authorized-leafs",
			secretPrefix:  "nats-leaf-nkey",
		}}
		return g.writeConfigMap(context.Background(), users)
	})

	if gotRV != "42" {
		t.Fatalf("Update carried resourceVersion %q, want %q (the fetched object's)", gotRV, "42")
	}
}
