/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package webhook provides admission webhooks for the sqlite-operator.
package webhook

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

var webhookLog = logf.Log.WithName("sqlitedb-webhook")

// SQLiteDBValidator implements admission.Validator[*databasev1.SQLiteDB].
// Client is injected by SetupWithManager and used for optional replica-count checks.
// When nil (e.g. in unit tests), the replica check is skipped.
type SQLiteDBValidator struct {
	Client client.Client
}

// SetupWithManager registers the validating webhook with the controller manager.
func (v *SQLiteDBValidator) SetupWithManager(mgr ctrl.Manager) error {
	v.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr, &databasev1.SQLiteDB{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates a new SQLiteDB resource.
func (v *SQLiteDBValidator) ValidateCreate(ctx context.Context, db *databasev1.SQLiteDB) (admission.Warnings, error) {
	webhookLog.Info("ValidateCreate", "name", db.Name)
	return validateSQLiteDB(ctx, v.Client, db)
}

// ValidateUpdate validates an update to a SQLiteDB resource.
func (v *SQLiteDBValidator) ValidateUpdate(ctx context.Context, _ *databasev1.SQLiteDB, newDB *databasev1.SQLiteDB) (admission.Warnings, error) {
	webhookLog.Info("ValidateUpdate", "name", newDB.Name)
	return validateSQLiteDB(ctx, v.Client, newDB)
}

// ValidateDelete is a no-op — deletion is always permitted.
func (v *SQLiteDBValidator) ValidateDelete(_ context.Context, _ *databasev1.SQLiteDB) (admission.Warnings, error) {
	return nil, nil
}

// validateSQLiteDB runs field-level validation common to create and update.
// It also emits admission warnings for configurations that are technically valid
// but carry operational risk.
func validateSQLiteDB(ctx context.Context, c client.Client, db *databasev1.SQLiteDB) (admission.Warnings, error) {
	var (
		errs     field.ErrorList
		warnings admission.Warnings
	)
	spec := field.NewPath("spec")

	// Exactly one of targetDeployment / targetStatefulSet must be set.
	bothSet := db.Spec.TargetDeployment != "" && db.Spec.TargetStatefulSet != ""
	neitherSet := db.Spec.TargetDeployment == "" && db.Spec.TargetStatefulSet == ""

	switch {
	case bothSet:
		errs = append(errs, field.Invalid(
			spec.Child("targetStatefulSet"), db.Spec.TargetStatefulSet,
			"targetStatefulSet and targetDeployment are mutually exclusive; set exactly one"))
	case neitherSet:
		errs = append(errs, field.Required(
			spec.Child("targetDeployment"),
			"one of targetDeployment or targetStatefulSet must reference an existing workload"))
	}

	if db.Spec.DatabaseName == "" {
		errs = append(errs, field.Required(spec.Child("databaseName"),
			"databaseName must be set (e.g. \"app.db\")"))
	}

	if db.Spec.DatabasePath == "" {
		errs = append(errs, field.Required(spec.Child("databasePath"),
			"databasePath must be set (e.g. \"/data\")"))
	}

	if db.Spec.Backup.Enabled {
		backupPath := spec.Child("backup")
		dest := db.Spec.Backup.Destination

		if dest.S3 == nil {
			errs = append(errs, field.Required(backupPath.Child("destination", "s3"),
				"backup.destination.s3 is required when backup.enabled is true"))
		} else {
			s3Path := backupPath.Child("destination", "s3")
			if dest.S3.Bucket == "" {
				errs = append(errs, field.Required(s3Path.Child("bucket"), "bucket must be set"))
			}
			if dest.S3.SecretRef == "" {
				errs = append(errs, field.Required(s3Path.Child("secretRef"),
					"secretRef must name a Secret containing S3 credentials"))
			}
		}
	}

	// Warn when autoRestore is enabled — known upstream corruption risk.
	if db.Spec.Backup.AutoRestore {
		warnings = append(warnings,
			"autoRestore: true carries a known restore-corruption risk (Litestream upstream #1164/#1220). "+
				"The integrity gate (PRAGMA quick_check) will catch corruption before the app starts, "+
				"but consider using a SQLiteRestore CR for controlled, auditable recovery instead.")
	}

	// Optional replica-count check: requires a live API client and a resolvable workload.
	// Skip the check when the client is nil (unit tests) or the workload is not yet created.
	if c != nil && !bothSet && !neitherSet {
		if err := checkWorkloadReplicas(ctx, c, db, spec, &errs); err != nil {
			// Unexpected API error — log and continue rather than blocking the admission.
			webhookLog.Error(err, "replica count check failed; skipping", "name", db.Name)
		}
	}

	if len(errs) > 0 {
		return warnings, fmt.Errorf("%w", errs.ToAggregate())
	}
	return warnings, nil
}

// checkWorkloadReplicas looks up the target workload and appends a validation
// error if it has more than one replica. Not-found errors are silently ignored
// so that a SQLiteDB can be created before the target workload exists.
func checkWorkloadReplicas(ctx context.Context, c client.Client, db *databasev1.SQLiteDB, spec *field.Path, errs *field.ErrorList) error {
	var replicas int32

	if db.Spec.TargetStatefulSet != "" {
		ss := &appsv1.StatefulSet{}
		if err := c.Get(ctx, types.NamespacedName{
			Namespace: db.Namespace,
			Name:      db.Spec.TargetStatefulSet,
		}, ss); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // workload not yet created — defer check to reconciler
			}
			return err
		}
		if ss.Spec.Replicas != nil {
			replicas = *ss.Spec.Replicas
		} else {
			replicas = 1
		}
		if replicas > 1 {
			*errs = append(*errs, field.Invalid(
				spec.Child("targetStatefulSet"), db.Spec.TargetStatefulSet,
				fmt.Sprintf("target StatefulSet has %d replicas; Litestream requires exactly 1 writer", replicas)))
		}
		return nil
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: db.Namespace,
		Name:      db.Spec.TargetDeployment,
	}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	} else {
		replicas = 1
	}
	if replicas > 1 {
		*errs = append(*errs, field.Invalid(
			spec.Child("targetDeployment"), db.Spec.TargetDeployment,
			fmt.Sprintf("target Deployment has %d replicas; Litestream requires exactly 1 writer", replicas)))
	}
	return nil
}
