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

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

var webhookLog = logf.Log.WithName("sqlitedb-webhook")

// SQLiteDBValidator implements admission.Validator[*databasev1.SQLiteDB].
type SQLiteDBValidator struct{}

// SetupWithManager registers the validating webhook with the controller manager.
func (v *SQLiteDBValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &databasev1.SQLiteDB{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates a new SQLiteDB resource.
func (v *SQLiteDBValidator) ValidateCreate(_ context.Context, db *databasev1.SQLiteDB) (admission.Warnings, error) {
	webhookLog.Info("ValidateCreate", "name", db.Name)
	return nil, validateSQLiteDB(db)
}

// ValidateUpdate validates an update to a SQLiteDB resource.
func (v *SQLiteDBValidator) ValidateUpdate(_ context.Context, _ *databasev1.SQLiteDB, newDB *databasev1.SQLiteDB) (admission.Warnings, error) {
	webhookLog.Info("ValidateUpdate", "name", newDB.Name)
	return nil, validateSQLiteDB(newDB)
}

// ValidateDelete is a no-op — deletion is always permitted.
func (v *SQLiteDBValidator) ValidateDelete(_ context.Context, _ *databasev1.SQLiteDB) (admission.Warnings, error) {
	return nil, nil
}

// validateSQLiteDB runs field-level validation common to create and update.
func validateSQLiteDB(db *databasev1.SQLiteDB) error {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if db.Spec.TargetDeployment == "" {
		errs = append(errs, field.Required(spec.Child("targetDeployment"),
			"targetDeployment must reference an existing Deployment in the same namespace"))
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

	if len(errs) > 0 {
		return fmt.Errorf("%w", errs.ToAggregate())
	}
	return nil
}
