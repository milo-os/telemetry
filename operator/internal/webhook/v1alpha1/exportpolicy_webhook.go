// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	telemetryv1alpha1 "go.datum.net/o11y/operator/api/v1alpha1"
	"go.datum.net/o11y/operator/internal/validation"
)

// nolint:unused
// log is for logging in this package.
var exportpolicylog = logf.Log.WithName("exportpolicy-resource")

// SetupExportPolicyWebhookWithManager registers the webhook for ExportPolicy in the manager.
func SetupExportPolicyWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &telemetryv1alpha1.ExportPolicy{}).
		WithValidator(&ExportPolicyCustomValidator{}).
		Complete()
}

// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-telemetry-datumapis-com-v1alpha1-exportpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=telemetry.miloapis.com,resources=exportpolicies,verbs=create;update,versions=v1alpha1,name=vexportpolicy-v1alpha1.kb.io,admissionReviewVersions=v1

// ExportPolicyCustomValidator struct is responsible for validating the ExportPolicy resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ExportPolicyCustomValidator struct {
}

var _ admission.Validator[*telemetryv1alpha1.ExportPolicy] = &ExportPolicyCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type ExportPolicy.
func (v *ExportPolicyCustomValidator) ValidateCreate(ctx context.Context, exportpolicy *telemetryv1alpha1.ExportPolicy) (admission.Warnings, error) {
	exportpolicylog.Info("Validation for ExportPolicy upon creation", "name", exportpolicy.GetName())

	if errs := validation.ValidateExportPolicy(exportpolicy); len(errs) > 0 {
		return nil, errors.NewInvalid(exportpolicy.GetObjectKind().GroupVersionKind().GroupKind(), exportpolicy.Name, errs)
	}

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type ExportPolicy.
func (v *ExportPolicyCustomValidator) ValidateUpdate(ctx context.Context, oldObj, exportpolicy *telemetryv1alpha1.ExportPolicy) (admission.Warnings, error) {
	exportpolicylog.Info("Validation for ExportPolicy upon update", "name", exportpolicy.GetName())

	if errs := validation.ValidateExportPolicy(exportpolicy); len(errs) > 0 {
		return nil, errors.NewInvalid(exportpolicy.GetObjectKind().GroupVersionKind().GroupKind(), exportpolicy.Name, errs)
	}

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type ExportPolicy.
func (v *ExportPolicyCustomValidator) ValidateDelete(ctx context.Context, exportpolicy *telemetryv1alpha1.ExportPolicy) (admission.Warnings, error) {
	exportpolicylog.Info("Validation for ExportPolicy upon deletion", "name", exportpolicy.GetName())

	return nil, nil
}
