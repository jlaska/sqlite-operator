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

// Package controller contains the SQLite database controller implementation.
package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

// Expose annotation keys as package-level aliases so tests can reference them
// without importing the API package directly.
const (
	injectAnnotation = databasev1.AnnotationInject
	configAnnotation = databasev1.AnnotationConfig
)

// SQLiteDBReconciler reconciles a SQLiteDB object
type SQLiteDBReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *SQLiteDBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	sqliteDB := &databasev1.SQLiteDB{}
	if err := r.Get(ctx, req.NamespacedName, sqliteDB); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling SQLiteDB", "target", sqliteDB.Spec.TargetDeployment)

	// Manage the Litestream ConfigMap that the injected sidecar will mount.
	if err := r.reconcileLitestreamConfig(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile Litestream ConfigMap")
		return ctrl.Result{}, err
	}

	// Annotate the target Deployment to trigger sidecar injection on next rollout.
	if err := r.reconcileTargetAnnotation(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to annotate target Deployment")
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to update SQLiteDB status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileLitestreamConfig creates or updates the ConfigMap that holds the
// litestream.yml file mounted into the injected sidecar container.
func (r *SQLiteDBReconciler) reconcileLitestreamConfig(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sqliteDB.Name + "-litestream",
			Namespace: sqliteDB.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"litestream.yml": r.buildLitestreamConfig(sqliteDB),
		}
		return controllerutil.SetControllerReference(sqliteDB, cm, r.Scheme)
	})

	return err
}

// buildLitestreamConfig renders the litestream.yml content for the given CR.
func (r *SQLiteDBReconciler) buildLitestreamConfig(sqliteDB *databasev1.SQLiteDB) string {
	dbPath := fmt.Sprintf("%s/%s", sqliteDB.Spec.DatabasePath, sqliteDB.Spec.DatabaseName)

	cfg := fmt.Sprintf("dbs:\n  - path: %s\n    replicas:\n", dbPath)

	if sqliteDB.Spec.Backup.Enabled && sqliteDB.Spec.Backup.Destination.S3 != nil {
		s3 := sqliteDB.Spec.Backup.Destination.S3
		cfg += "      - type: s3\n"
		if s3.Endpoint != "" {
			cfg += fmt.Sprintf("        endpoint: %s\n", s3.Endpoint)
		}
		cfg += fmt.Sprintf("        bucket: %s\n", s3.Bucket)
		if s3.Path != "" {
			cfg += fmt.Sprintf("        path: %s\n", s3.Path)
		}
		if sqliteDB.Spec.Backup.Schedule != "" {
			cfg += fmt.Sprintf("        snapshot-interval: %s\n", sqliteDB.Spec.Backup.Schedule)
		}
		if sqliteDB.Spec.Backup.Retention.Count > 0 {
			cfg += fmt.Sprintf("        retention: %d\n", sqliteDB.Spec.Backup.Retention.Count)
		}
	}

	return cfg
}

// reconcileTargetAnnotation adds the injection and config annotations to the
// target Deployment's pod template. Annotating the pod template causes the
// Deployment controller to perform a rolling restart, so new pods inherit the
// annotations and the mutating webhook can inject the Litestream sidecar.
func (r *SQLiteDBReconciler) reconcileTargetAnnotation(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: sqliteDB.Namespace,
		Name:      sqliteDB.Spec.TargetDeployment,
	}, deployment); err != nil {
		return fmt.Errorf("target Deployment %q not found: %w", sqliteDB.Spec.TargetDeployment, err)
	}

	configRef := fmt.Sprintf("%s/%s", sqliteDB.Namespace, sqliteDB.Name)

	const injectEnabled = "true"

	// Only patch when annotations are actually missing to avoid an infinite
	// rolling-restart loop on every reconcile.
	tmplAnnotations := deployment.Spec.Template.Annotations
	if tmplAnnotations[injectAnnotation] == injectEnabled && tmplAnnotations[configAnnotation] == configRef {
		return nil
	}

	patch := client.MergeFrom(deployment.DeepCopy())

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations[injectAnnotation] = injectEnabled
	deployment.Spec.Template.Annotations[configAnnotation] = configRef

	return r.Patch(ctx, deployment, patch)
}

// updateStatus computes and writes the status subresource for the SQLiteDB.
func (r *SQLiteDBReconciler) updateStatus(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	// Check whether the target Deployment exists and has the annotation applied.
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: sqliteDB.Namespace,
		Name:      sqliteDB.Spec.TargetDeployment,
	}, deployment)

	now := metav1.Now()
	sqliteDB.Status.ObservedGeneration = sqliteDB.Generation

	if err != nil {
		sqliteDB.Status.Phase = databasev1.PhaseError
		sqliteDB.Status.Ready = false
		meta.SetStatusCondition(&sqliteDB.Status.Conditions, metav1.Condition{
			Type:               databasev1.ConditionSidecarInjected,
			Status:             metav1.ConditionFalse,
			Reason:             "DeploymentNotFound",
			Message:            fmt.Sprintf("target Deployment %q not found", sqliteDB.Spec.TargetDeployment),
			ObservedGeneration: sqliteDB.Generation,
			LastTransitionTime: now,
		})
		meta.SetStatusCondition(&sqliteDB.Status.Conditions, metav1.Condition{
			Type:               databasev1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "DeploymentNotFound",
			Message:            "target Deployment not found",
			ObservedGeneration: sqliteDB.Generation,
			LastTransitionTime: now,
		})
	} else {
		annotated := deployment.Spec.Template.Annotations[injectAnnotation] == "true"
		injectedCondStatus := metav1.ConditionFalse
		injectedReason := "AnnotationPending"
		injectedMsg := "injection annotation not yet observed on target Deployment"
		if annotated {
			injectedCondStatus = metav1.ConditionTrue
			injectedReason = "Annotated"
			injectedMsg = "target Deployment is annotated for sidecar injection"
		}
		meta.SetStatusCondition(&sqliteDB.Status.Conditions, metav1.Condition{
			Type:               databasev1.ConditionSidecarInjected,
			Status:             injectedCondStatus,
			Reason:             injectedReason,
			Message:            injectedMsg,
			ObservedGeneration: sqliteDB.Generation,
			LastTransitionTime: now,
		})

		if annotated && deployment.Status.ReadyReplicas > 0 {
			sqliteDB.Status.Phase = databasev1.PhaseReady
			sqliteDB.Status.Ready = true
			meta.SetStatusCondition(&sqliteDB.Status.Conditions, metav1.Condition{
				Type:               databasev1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "DeploymentReady",
				Message:            "target Deployment has ready replicas",
				ObservedGeneration: sqliteDB.Generation,
				LastTransitionTime: now,
			})
		} else {
			sqliteDB.Status.Phase = databasev1.PhasePending
			sqliteDB.Status.Ready = false
			meta.SetStatusCondition(&sqliteDB.Status.Conditions, metav1.Condition{
				Type:               databasev1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "DeploymentNotReady",
				Message:            "waiting for target Deployment to have ready replicas",
				ObservedGeneration: sqliteDB.Generation,
				LastTransitionTime: now,
			})
		}
	}

	return r.Status().Update(ctx, sqliteDB)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SQLiteDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.SQLiteDB{}).
		Owns(&corev1.ConfigMap{}).
		Named("sqlitedb").
		Complete(r)
}
