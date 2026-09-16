// SPDX-License-Identifier: AGPL-3.0-only

// Command nkey-generator provisions per-PoP NATS leaf NKey credentials for the
// o11y telemetry hub. It runs on a schedule in the hub cluster and, for every
// Karmada Cluster labelled telemetry.miloapis.com/nats-leaf=enabled:
//
//   - generates and stores a NATS NKey (seed + public key) in a Secret named
//     nats-leaf-nkey-<cluster> if it does not yet exist (a regenerated seed
//     would break that PoP until the next ESO refresh), and
//   - maintains the ESO PushSecret that mirrors that seed to GCP Secret
//     Manager, and
//   - rewrites the hub's authorized-leafs ConfigMap which the hub HelmRelease
//     consumes via valuesFrom so the per-PoP nkey users land in the O11Y
//     account.
//
// The Karmada API is reached using a secretless kubeconfig whose tokenFile
// points at a projected ServiceAccount token.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/nats-io/nkeys"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const (
	clusterLabel      = "telemetry.miloapis.com/nats-leaf"
	clusterLabelValue = "enabled"
)

// clusterNameRE matches a single DNS label. Karmada Cluster names allow
// dots, which would shift nkeyUser's subject token count if interpolated
// unchecked.
var clusterNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func validClusterName(name string) bool {
	return clusterNameRE.MatchString(name)
}

var (
	secretsGVR    = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	configMapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	clustersGVR   = schema.GroupVersionResource{
		Group: "cluster.karmada.io", Version: "v1alpha1", Resource: "clusters",
	}
	pushSecretsGVR = schema.GroupVersionResource{
		Group: "external-secrets.io", Version: "v1alpha1", Resource: "pushsecrets",
	}
)

type config struct {
	namespace         string
	karmadaKubeconfig string
	secretPrefix      string
	configMapName     string
	secretStoreName   string
	secretStoreKind   string
}

type generator struct {
	local kubernetes.Interface
	dyn   dynamic.Interface
	cfg   config
}

func configFromEnv() (config, error) {
	c := config{
		namespace:         envOr("NAMESPACE", "o11y-system"),
		karmadaKubeconfig: os.Getenv("KARMADA_KUBECONFIG"),
		secretPrefix:      envOr("SECRET_PREFIX", "nats-leaf-nkey"),
		configMapName:     envOr("AUTHORIZED_LEAFS_CONFIGMAP", "nats-o11y-authorized-leafs"),
		secretStoreName:   envOr("SECRET_STORE_NAME", "gcp-secret-store"),
		secretStoreKind:   envOr("SECRET_STORE_KIND", "ClusterSecretStore"),
	}
	if c.karmadaKubeconfig == "" {
		return config{}, fmt.Errorf("missing required env var KARMADA_KUBECONFIG")
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("generator failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}

	ctx := context.Background()

	kc, err := clientcmd.BuildConfigFromFlags("", cfg.karmadaKubeconfig)
	if err != nil {
		return fmt.Errorf("build karmada config from %s: %w", cfg.karmadaKubeconfig, err)
	}
	karmadaDyn, err := dynamic.NewForConfig(kc)
	if err != nil {
		return fmt.Errorf("build karmada dynamic client: %w", err)
	}

	lc, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("build local in-cluster config: %w", err)
	}
	localClientset, err := kubernetes.NewForConfig(lc)
	if err != nil {
		return fmt.Errorf("build local clientset: %w", err)
	}
	localDyn, err := dynamic.NewForConfig(lc)
	if err != nil {
		return fmt.Errorf("build local dynamic client: %w", err)
	}

	g := &generator{
		local: localClientset,
		dyn:   localDyn,
		cfg:   cfg,
	}

	return g.sync(ctx, karmadaDyn)
}

// sync lists the enabled Karmada clusters, provisions their keys, and rewrites
// the authorized-leafs ConfigMap. Any Karmada API failure aborts so a blip
// never silently truncates the published user list.
func (g *generator) sync(ctx context.Context, karmada dynamic.Interface) error {
	clusters, err := enabledClusters(ctx, karmada)
	if err != nil {
		return fmt.Errorf("list karmada clusters: %w", err)
	}

	users := staticUsers()
	for _, name := range clusters {
		if !validClusterName(name) {
			slog.Warn("skipping cluster with invalid name for NATS subject use", "cluster", name)
			continue
		}
		pub, err := g.ensureKey(ctx, name)
		if err != nil {
			return err
		}
		users = append(users, nkeyUser(name, pub))
	}

	if len(users) == 0 {
		return fmt.Errorf("refusing to write an empty authorized-leafs users list")
	}

	if err := g.writeConfigMap(ctx, users); err != nil {
		return err
	}

	slog.Info("generator sync complete", "clusters", clusters)
	return nil
}

// staticUsers returns the certificate-mapped O11Y account users that never
// change with PoP membership. Names are the mapped certificate subjects
// (verify_and_map), not passwords.
func staticUsers() []map[string]any {
	return []map[string]any{
		permissionsMap(
			map[string]string{"user": "CN=nack.nats.client"},
			[]string{">"}, []string{">"},
		),
		// $JS.ACK.> is where a pull consumer publishes its acks; without it
		// the sink pulls but never acks and the stream only grows.
		permissionsMap(
			map[string]string{"user": "CN=o11y-sink-nats-client"},
			[]string{"$JS.API.>", "$JS.ACK.>"}, []string{"o11y.>", "_INBOX.>"},
		),
	}
}

func permissionsMap(identity map[string]string, publish, subscribe []string) map[string]any {
	m := map[string]any{
		"permissions": map[string]any{
			"publish":   map[string]any{"allow": toAny(publish)},
			"subscribe": map[string]any{"allow": toAny(subscribe)},
		},
	}
	for k, v := range identity {
		m[k] = v
	}
	return m
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// enabledClusters returns the names of Karmada Clusters labelled
// telemetry.miloapis.com/nats-leaf=enabled. Any error aborts so the caller never
// acts on a partial list.
func enabledClusters(ctx context.Context, karmada dynamic.Interface) ([]string, error) {
	list, err := karmada.Resource(clustersGVR).List(ctx, metav1.ListOptions{
		LabelSelector: clusterLabel + "=" + clusterLabelValue,
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, c := range list.Items {
		names = append(names, c.GetName())
	}
	return names, nil
}

// ensureKey ensures the per-PoP NKey Secret and its ESO PushSecret exist and
// returns the NKey public key. A seed is never regenerated once stored.
func (g *generator) ensureKey(ctx context.Context, cluster string) (string, error) {
	secretName := g.cfg.secretPrefix + "-" + cluster

	secret, err := g.local.CoreV1().Secrets(g.cfg.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		pub := string(secret.Data["public"])
		if pub == "" {
			pub = string(secret.Data["nkey"])
		}
		if pub == "" {
			return "", fmt.Errorf("secret %s exists but has no public key", secretName)
		}
		if err := g.ensurePushSecret(ctx, cluster, secretName); err != nil {
			return "", err
		}
		return pub, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get secret %s: %w", secretName, err)
	}

	kp, err := generateNKey()
	if err != nil {
		return "", fmt.Errorf("generate nkey for %s: %w", cluster, err)
	}

	seedObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      secretName,
			"namespace": g.cfg.namespace,
			"labels": map[string]any{
				clusterLabel:                     clusterLabelValue,
				"telemetry.miloapis.com/cluster": cluster,
			},
		},
		"type": "Opaque",
		"stringData": map[string]any{
			"seed":   string(kp.Seed),
			"public": kp.Public,
			"nkey":   kp.Public,
		},
	}}

	res := g.dyn.Resource(secretsGVR).Namespace(g.cfg.namespace)
	if _, err := res.Create(ctx, seedObj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create secret %s: %w", secretName, err)
	}

	slog.Info("provisioned nkey", "cluster", cluster, "secret", secretName)
	if err := g.ensurePushSecret(ctx, cluster, secretName); err != nil {
		return "", err
	}
	return kp.Public, nil
}

// ensurePushSecret upserts the ESO PushSecret that mirrors the per-PoP seed
// Secret to GCP Secret Manager under the same name. The generator never talks
// to GCP directly; IAM stays with ESO.
func (g *generator) ensurePushSecret(ctx context.Context, cluster, secretName string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1alpha1",
		"kind":       "PushSecret",
		"metadata": map[string]any{
			"name":      secretName,
			"namespace": g.cfg.namespace,
		},
		"spec": map[string]any{
			"updatePolicy":    "Replace",
			"deletionPolicy":  "None",
			"refreshInterval": "1h",
			"secretStoreRefs": []any{
				map[string]any{"name": g.cfg.secretStoreName, "kind": g.cfg.secretStoreKind},
			},
			"selector": map[string]any{
				"secret": map[string]any{"name": secretName},
			},
			"data": []any{
				map[string]any{
					"match": map[string]any{
						"remoteRef": map[string]any{"remoteKey": secretName},
					},
				},
			},
		},
	}}

	res := g.dyn.Resource(pushSecretsGVR).Namespace(g.cfg.namespace)
	if existing, err := res.Get(ctx, secretName, metav1.GetOptions{}); err == nil {
		obj.SetResourceVersion(existing.GetResourceVersion())
		_, err := res.Update(ctx, obj, metav1.UpdateOptions{})
		return err
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get pushsecret %s: %w", secretName, err)
	}
	_, err := res.Create(ctx, obj, metav1.CreateOptions{})
	return err
}

// nkeyUser builds the narrow per-PoP leaf NKey user block. Never widen these
// to ">" or grant $JS.API.* -- JetStream API access lets a holder create a
// consumer over another PoP's subjects and read it back via _INBOX.>,
// bypassing the publish scoping below entirely.
func nkeyUser(cluster, publicKey string) map[string]any {
	return permissionsMap(
		map[string]string{"nkey": publicKey},
		[]string{
			"o11y.logs." + cluster + ".*",
			"o11y.metrics." + cluster + ".*",
			"o11y.traces." + cluster + ".*",
		},
		[]string{"_INBOX.>"},
	)
}

// droppedNkeys returns the cluster names of entries present in prev but
// absent from next, excluding any whose per-cluster Secret has already been
// deleted. That deletion is the operator's explicit signal that dropping the
// nkey is an intentional PoP decommission rather than a transient Karmada
// listing glitch -- without this check the safety net below could never be
// satisfied: prev is read back from the very ConfigMap this function guards,
// so a decommissioned cluster would be "dropped" forever with no way to
// clear it. Static CN-mapped users have no "nkey" field and are excluded.
func (g *generator) droppedNkeys(ctx context.Context, prev, next []map[string]any) ([]string, error) {
	have := make(map[string]bool, len(next))
	for _, u := range next {
		if pub, ok := u["nkey"].(string); ok {
			have[pub] = true
		}
	}
	var dropped []string
	for _, u := range prev {
		pub, ok := u["nkey"].(string)
		if !ok || have[pub] {
			continue
		}
		cluster := clusterFromUser(u, pub)
		secretName := g.cfg.secretPrefix + "-" + cluster
		_, err := g.local.CoreV1().Secrets(g.cfg.namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			dropped = append(dropped, cluster)
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("checking secret %s for dropped cluster %s: %w", secretName, cluster, err)
		}
	}
	return dropped, nil
}

// clusterFromUser recovers the cluster name nkeyUser embeds in the logs
// publish subject (o11y.logs.<cluster>.*), falling back to the raw public
// key if that subject shape ever changes -- callers only use this for
// display, so a degraded fallback is safe.
func clusterFromUser(u map[string]any, fallback string) string {
	perms, ok := u["permissions"].(map[string]any)
	if !ok {
		return fallback
	}
	publish, ok := perms["publish"].(map[string]any)
	if !ok {
		return fallback
	}
	allow, ok := publish["allow"].([]any)
	if !ok {
		return fallback
	}
	for _, s := range allow {
		subj, ok := s.(string)
		if !ok {
			continue
		}
		rest, ok := strings.CutPrefix(subj, "o11y.logs.")
		if !ok {
			continue
		}
		if cluster, ok := strings.CutSuffix(rest, ".*"); ok {
			return cluster
		}
	}
	return fallback
}

// previousUsers reads the currently-applied ConfigMap's accounts.O11Y.users
// list. A missing ConfigMap or unparseable values.yaml is treated as "no
// previous users" -- nothing to protect against shrinking yet.
func (g *generator) previousUsers(ctx context.Context, name string) []map[string]any {
	res := g.dyn.Resource(configMapsGVR).Namespace(g.cfg.namespace)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	raw, found, err := unstructured.NestedString(obj.Object, "data", "values.yaml")
	if err != nil || !found {
		return nil
	}
	var doc struct {
		Config struct {
			Merge struct {
				Accounts struct {
					O11Y struct {
						Users []map[string]any `json:"users"`
					} `json:"O11Y"`
				} `json:"accounts"`
			} `json:"merge"`
		} `json:"config"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil
	}
	return doc.Config.Merge.Accounts.O11Y.Users
}

// writeConfigMap rewrites the authorized-leafs ConfigMap the hub HelmRelease
// reads via valuesFrom. data values.yaml carries the full O11Y account block,
// so the generator is the single source of truth for account users. Before
// writing, it refuses to silently drop a previously-authorized leaf nkey --
// a transient Karmada listing glitch must not deauthorize a live PoP without
// a human noticing.
func (g *generator) writeConfigMap(ctx context.Context, users []map[string]any) error {
	prev := g.previousUsers(ctx, g.cfg.configMapName)
	dropped, err := g.droppedNkeys(ctx, prev, users)
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		return fmt.Errorf("refusing to write authorized-leafs: cluster(s) %v would be dropped but their nkey Secret %s-<cluster> still exists (delete it once the PoP decommission is confirmed, then retry)", dropped, g.cfg.secretPrefix)
	}

	valuesDoc := map[string]any{
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

	valuesBytes, err := yaml.Marshal(valuesDoc)
	if err != nil {
		return fmt.Errorf("marshal values.yaml: %w", err)
	}

	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      g.cfg.configMapName,
			"namespace": g.cfg.namespace,
			"labels": map[string]any{
				"reconcile.fluxcd.io/watch": "Enabled",
			},
		},
		"data": map[string]any{
			"values.yaml": string(valuesBytes),
		},
	}}

	res := g.dyn.Resource(configMapsGVR).Namespace(g.cfg.namespace)
	if existing, err := res.Get(ctx, g.cfg.configMapName, metav1.GetOptions{}); err == nil {
		cm.SetResourceVersion(existing.GetResourceVersion())
		_, err := res.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get configmap %s: %w", g.cfg.configMapName, err)
	}
	_, err = res.Create(ctx, cm, metav1.CreateOptions{})
	return err
}

type nkeyPair struct {
	Seed   []byte
	Public string
}

// generateNKey creates a new user-scoped NATS NKey. The returned seed is an
// independent copy; the temporary KeyPair is wiped on return.
func generateNKey() (nkeyPair, error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nkeyPair{}, err
	}
	defer kp.Wipe()

	seed, err := kp.Seed()
	if err != nil {
		return nkeyPair{}, err
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return nkeyPair{}, err
	}
	return nkeyPair{Seed: append([]byte(nil), seed...), Public: pub}, nil
}
