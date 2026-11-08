// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"go.datum.net/o11y/operator/api/v1alpha1"
)

const (
	exportPolicyLabelDomain = "exportpolicy.telemetry.miloapis.com"

	exportPolicyNameLabel      = exportPolicyLabelDomain + "/name"
	exportPolicyNamespaceLabel = exportPolicyLabelDomain + "/namespace"

	// exportPolicyControllerFinalizer is the finalizer added to ExportPolicy
	// objects to ensure the downstream vector config Secret is deleted before
	// the ExportPolicy is removed from the cluster.
	exportPolicyControllerFinalizer = exportPolicyLabelDomain + "/controller"

	// conditionTypeAccepted is the status condition type set on each sink
	// reflecting whether its configuration passed validation.
	conditionTypeAccepted = "Accepted"
)

// ExportPolicyReconciler reconciles a ExportPolicy object
type ExportPolicyReconciler struct {
	mgr mcmanager.Manager

	// The client for the downstream cluster that vector configurations will be
	// created in.
	DownstreamClient client.Client

	// The namespace in the downstream cluster that vector configurations will be
	// created in.
	DownstreamVectorConfigNamespace string

	// The metrics service that can be used to query metrics from the telemetry
	// system.
	MetricsService MetricsService

	// The vector config label key that will be added to the vector config secret.
	VectorConfigLabelKey   string
	VectorConfigLabelValue string

	// Finalizers manager
	finalizers finalizer.Finalizers
}

// MetricsService is a struct that contains the information needed to configure
// a metrics service.
type MetricsService struct {
	// The endpoint of the metrics service that can be used to query metrics from
	// the telemetry system.
	Endpoint string
	// The username for the metrics service.
	Username string
	// The password for the metrics service.
	Password string
}

// vectorSecretFinalizer handles deletion of the downstream Vector config Secret.
type vectorSecretFinalizer struct {
	downstreamClient                client.Client
	downstreamVectorConfigNamespace string
}

var _ finalizer.Finalizer = &vectorSecretFinalizer{}

// Finalize deletes the downstream Vector config secret associated with the ExportPolicy.
// It removes the finalizer from the object if the deletion is successful or the secret is not found.
func (f *vectorSecretFinalizer) Finalize(ctx context.Context, obj client.Object) (finalizer.Result, error) {
	logger := log.FromContext(ctx)
	exportPolicy, ok := obj.(*v1alpha1.ExportPolicy)
	if !ok {
		// Should not happen
		return finalizer.Result{}, fmt.Errorf("object %T is not an ExportPolicy", obj)
	}

	// Construct ObjectMeta for the secret to delete
	secretMeta := metav1.ObjectMeta{
		Name:      fmt.Sprintf("export-policy-vector-config-%s", exportPolicy.GetUID()),
		Namespace: f.downstreamVectorConfigNamespace,
	}
	secretToDelete := &corev1.Secret{ObjectMeta: secretMeta}

	logger.Info("attempting to delete downstream secret", "secret", client.ObjectKeyFromObject(secretToDelete))

	err := f.downstreamClient.Delete(ctx, secretToDelete)
	if err != nil && !errors.IsNotFound(err) {
		return finalizer.Result{}, fmt.Errorf("failed to delete downstream secret: %w", err)
	}

	if errors.IsNotFound(err) {
		logger.Info("downstream secret not found, proceeding")
	} else {
		logger.Info("successfully deleted downstream secret")
	}

	// Secret deleted or not found, finalization complete for this finalizer.
	// The finalizer.Finalizers manager will handle removing the finalizer string.
	return finalizer.Result{}, nil
}

// +kubebuilder:rbac:groups=telemetry.miloapis.com,resources=exportpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=telemetry.miloapis.com,resources=exportpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;create;update;delete

// Reconcile an Export Policy and ensure the necessary resources exist to export
// the telemetry sources that are configured. This will create a vector config
// secret that can be used to configure the vector exporter to export the
// telemetry sources to the configured sinks. The vector config secret will be
// named `export-policy-vector-config-<export-policy-uid>.json`. The vector
// config secret will be created in the same namespace as the export policy.
//
// For more details, check Reconcile and its Result here: -
// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.2/pkg/reconcile
func (r *ExportPolicyReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx, "project_name", req.ClusterName)
	ctx = log.IntoContext(ctx, logger)

	logger.Info("reconciling export policy")

	cluster, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get cluster: %w", err)
	}
	upstreamClient := cluster.GetClient()

	// Retrieve the export policy from the cluster and confirm if it exists.
	exportPolicy := &v1alpha1.ExportPolicy{}
	if err := upstreamClient.Get(ctx, req.NamespacedName, exportPolicy); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("export policy not found, assuming deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get export policy: %w", err)
	}

	// Finalize the export policy so we can handle the deletion of downstream
	// resources created by the controller.
	finalizeResult, err := r.finalizers.Finalize(ctx, exportPolicy)
	if err != nil {
		return ctrl.Result{}, err
	}

	if finalizeResult.Updated {
		logger.Info("finalizer updated the export policy object, updating API server")
		if updateErr := upstreamClient.Update(ctx, exportPolicy); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
	}

	// Don't process the export policy if it is marked for deletion.
	if !exportPolicy.DeletionTimestamp.IsZero() {
		logger.Info("export policy is marked for deletion, stopping reconciliation")
		return ctrl.Result{}, nil
	}

	// Validate that the export policy configuration is valid and update the
	// status of the export policy to reflect the status of the sinks.
	if reconcileExportPolicyStatus(ctx, upstreamClient, exportPolicy) {
		logger.Info("export policy status changed, updating status")
		if err := upstreamClient.Status().Update(ctx, exportPolicy); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update export policy status: %w", err)
		}
		// Status updated, requeue to ensure we work with the latest status
		return ctrl.Result{Requeue: true}, nil
	}

	// Create the vector configuration for the export policy. This will skip over
	// any source or sink configurations that are not valid.
	vectorConfig := r.createVectorConfiguration(ctx, strings.ReplaceAll(req.ClusterName.String(), "/", ""), upstreamClient, exportPolicy)
	vectorConfigJSON, err := json.MarshalIndent(vectorConfig, "", "  ")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to marshal vector config: %w", err)
	}

	// Create the downstream secret that will be used to configure the vector
	// exporter.
	configSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("export-policy-vector-config-%s", exportPolicy.GetUID()),
			Namespace: r.DownstreamVectorConfigNamespace,
			Labels: map[string]string{
				r.VectorConfigLabelKey:     r.VectorConfigLabelValue,
				exportPolicyNameLabel:      exportPolicy.Name,
				exportPolicyNamespaceLabel: exportPolicy.Namespace,
			},
		},
		Data: map[string][]byte{
			fmt.Sprintf("%s.json", exportPolicy.UID): vectorConfigJSON,
		},
	}

	logger.Info("creating or updating downstream secret")
	operationResult, err := controllerutil.CreateOrUpdate(ctx, r.DownstreamClient, configSecret, func() error {
		configSecret.Labels = map[string]string{
			r.VectorConfigLabelKey:     r.VectorConfigLabelValue,
			exportPolicyNameLabel:      exportPolicy.Name,
			exportPolicyNamespaceLabel: exportPolicy.Namespace,
		}
		configSecret.Data = map[string][]byte{
			fmt.Sprintf("%s.json", exportPolicy.UID): vectorConfigJSON,
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create or update downstream secret: %w", err)
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("downstream secret operation result", "operation", operationResult)
	}

	logger.Info("export policy reconciliation complete")
	return ctrl.Result{}, nil
}

// reconcileExportPolicyStatus validates the export policy configuration and
// updates the status of the export policy to reflect the status of the sinks.
func reconcileExportPolicyStatus(ctx context.Context, client client.Client, exportPolicy *v1alpha1.ExportPolicy) bool {
	statusChanged := false
	sinkStatuses := make([]v1alpha1.SinkStatus, 0, len(exportPolicy.Spec.Sinks))
	// Validate each of the sinks in the export policy have a valid configuration
	// and the secrets exist if necessary.
	for _, sink := range exportPolicy.Spec.Sinks {
		status := getSinkStatus(exportPolicy, sink.Name)

		if sink.Target.PrometheusRemoteWrite != nil {
			// Assume the sink is accepted and expect to be set to false if any
			// validation fails.
			accepted := true

			// Validate that any authentication for the sink is valid
			if sink.Target.PrometheusRemoteWrite.Authentication != nil {
				if sink.Target.PrometheusRemoteWrite.Authentication.BasicAuth != nil {
					_, err := retrieveBasicAuthSecret(ctx, client, sink.Target.PrometheusRemoteWrite.Authentication.BasicAuth.SecretRef, exportPolicy)
					if err != nil {
						accepted = false
						updated := apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
							Type:    conditionTypeAccepted,
							Status:  metav1.ConditionFalse,
							Reason:  "InvalidAuthentication",
							Message: err.Error(),
						})

						if updated {
							statusChanged = true
						}
					}
				}
			}

			if accepted {
				updated := apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
					Type:   conditionTypeAccepted,
					Status: metav1.ConditionTrue,
					Reason: "SinkConfigured",
				})

				if updated {
					statusChanged = true
				}
			}
		}

		sinkStatuses = append(sinkStatuses, *status)
	}

	exportPolicy.Status.Sinks = sinkStatuses

	// Update the overall status conditions of the export policy based on the
	// status of its sinks.
	return updateExportPolicyConditions(exportPolicy, sinkStatuses) || statusChanged
}

// getSinkStatus retrieves the existing sink status from the export policy if it
// exists, otherwise returns a new sink status with the given name
func getSinkStatus(exportPolicy *v1alpha1.ExportPolicy, sinkName string) *v1alpha1.SinkStatus {
	for _, existingStatus := range exportPolicy.Status.Sinks {
		if existingStatus.Name == sinkName {
			return existingStatus.DeepCopy()
		}
	}

	return &v1alpha1.SinkStatus{
		Name: sinkName,
	}
}

// updateExportPolicyStatus updates the overall status conditions of the
// export policy based on the status of its sinks. Returns true if conditions
// were changed.
func updateExportPolicyConditions(exportPolicy *v1alpha1.ExportPolicy, sinkStatuses []v1alpha1.SinkStatus) bool {
	var acceptedCount int
	for _, sinkStatus := range sinkStatuses {
		for _, condition := range sinkStatus.Conditions {
			if condition.Type == conditionTypeAccepted {
				if condition.Status == metav1.ConditionTrue {
					acceptedCount++
				}
			}
		}
	}

	var condition metav1.Condition
	if acceptedCount == len(sinkStatuses) {
		condition = metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "SinksAccepted",
			Message:            "All sinks are configured.",
			ObservedGeneration: exportPolicy.Generation,
		}
	} else {
		condition = metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "SinksNotAccepted",
			Message:            fmt.Sprintf("%d/%d sinks are accepted. Check the status of the sinks for more details.", acceptedCount, len(sinkStatuses)),
			ObservedGeneration: exportPolicy.Generation,
		}
	}

	return apimeta.SetStatusCondition(&exportPolicy.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ExportPolicyReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr

	// Initialize the finalizer manager
	r.finalizers = finalizer.NewFinalizers()

	// Create our custom finalizer implementation
	secretFinalizer := &vectorSecretFinalizer{
		downstreamClient:                r.DownstreamClient,
		downstreamVectorConfigNamespace: r.DownstreamVectorConfigNamespace,
	}

	// Register our custom finalizer
	if err := r.finalizers.Register(exportPolicyControllerFinalizer, secretFinalizer); err != nil {
		return fmt.Errorf("failed to register export policy controller finalizer: %w", err)
	}

	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.ExportPolicy{}, mcbuilder.WithEngageWithLocalCluster(false), mcbuilder.WithEngageWithProviderClusters(true)).
		Watches(&corev1.Secret{}, func(clusterName multicluster.ClusterName, cluster cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
			return mchandler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
				logger := log.FromContext(ctx)
				secret, ok := obj.(*corev1.Secret)
				if !ok {
					logger.Error(fmt.Errorf("object %T is not a Secret", obj), "unexpected type")
					return nil
				}

				// List all ExportPolicies in the same namespace as the secret.
				// Note: LocalSecretReference implies the secret is in the same namespace.
				policyList := &v1alpha1.ExportPolicyList{}
				if err := cluster.GetClient().List(ctx, policyList, client.InNamespace(secret.GetNamespace())); err != nil {
					logger.Error(err, "failed to list ExportPolicies", "namespace", secret.GetNamespace())
					return nil
				}

				var requests []mcreconcile.Request
				for _, policy := range policyList.Items {
					if referencesSecret(&policy, secret) {
						if requests == nil { // Initialize slice only if needed
							requests = make([]mcreconcile.Request, 0, 1) // Start with capacity 1
						}
						requests = append(requests, mcreconcile.Request{
							Request: reconcile.Request{
								NamespacedName: types.NamespacedName{
									Name:      policy.Name,
									Namespace: policy.Namespace,
								},
							},
						})
						// Log the enqueueing for clarity
						logger.V(1).Info("enqueuing ExportPolicy due to secret change", "exportpolicy", client.ObjectKeyFromObject(&policy), "secret", client.ObjectKeyFromObject(secret))
					}
				}

				return requests
			})(clusterName, cluster)
		}).
		Named("exportpolicy").
		Complete(r)
}

// referencesSecret checks if the given ExportPolicy references the provided Secret.
func referencesSecret(policy *v1alpha1.ExportPolicy, secret *corev1.Secret) bool {
	for _, sink := range policy.Spec.Sinks {
		if sink.Target != nil &&
			sink.Target.PrometheusRemoteWrite != nil &&
			sink.Target.PrometheusRemoteWrite.Authentication != nil &&
			sink.Target.PrometheusRemoteWrite.Authentication.BasicAuth != nil &&
			sink.Target.PrometheusRemoteWrite.Authentication.BasicAuth.SecretRef.Name == secret.Name {
			// Found a reference in the same namespace
			return true
		}
		// Add checks for other potential secret references here if needed in the future
	}
	return false
}
