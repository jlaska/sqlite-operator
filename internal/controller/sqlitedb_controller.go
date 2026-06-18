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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

// statusSyncInterval is how often the controller re-checks backup health even
// when no resource change event has fired.
const statusSyncInterval = 2 * time.Minute

// Expose annotation keys as package-level aliases so tests can reference them
// without importing the API package directly.
const (
	injectAnnotation = databasev1.AnnotationInject
	configAnnotation = databasev1.AnnotationConfig
)

// SQLiteDBReconciler reconciles a SQLiteDB object
type SQLiteDBReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

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

	if err := r.reconcileLitestreamConfig(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile Litestream ConfigMap")
		return ctrl.Result{}, err
	}

	if err := r.reconcileTargetAnnotation(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to annotate target Deployment")
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to update SQLiteDB status")
		return ctrl.Result{}, err
	}

	// Requeue periodically to refresh backup health even without change events.
	return ctrl.Result{RequeueAfter: statusSyncInterval}, nil
}

// reconcileLitestreamConfig creates or updates the ConfigMap holding litestream.yml.
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

// reconcileTargetAnnotation adds injection annotations to the target
// Deployment's pod template, triggering a rolling restart so new pods
// inherit them and the mutating webhook can inject the Litestream sidecar.
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

// litestreamContainerState inspects pods belonging to the target Deployment
// and returns the state of the Litestream sidecar container across all
// running pods. It returns (healthy, message) where healthy is true only
// when at least one pod has the sidecar in a Running state and no pods have
// it in a terminal failure state (CrashLoopBackOff / OOMKilled / Error).
func (r *SQLiteDBReconciler) litestreamContainerState(ctx context.Context, sqliteDB *databasev1.SQLiteDB, deployment *appsv1.Deployment) (healthy bool, message string) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(sqliteDB.Namespace),
		client.MatchingLabels(deployment.Spec.Selector.MatchLabels),
	); err != nil {
		return false, fmt.Sprintf("failed to list pods: %v", err)
	}

	var (
		runningCount int
		failedPods   []string
		noneFound    = true
	)

	for i := range podList.Items {
		pod := &podList.Items[i]
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "litestream" {
				continue
			}
			noneFound = false
			if cs.State.Running != nil {
				runningCount++
			}
			if cs.State.Waiting != nil && isFailureReason(cs.State.Waiting.Reason) {
				failedPods = append(failedPods, fmt.Sprintf("%s (%s)", pod.Name, cs.State.Waiting.Reason))
			}
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
				failedPods = append(failedPods, fmt.Sprintf("%s (exit %d)", pod.Name, cs.State.Terminated.ExitCode))
			}
		}
	}

	switch {
	case noneFound:
		return false, "no pods with Litestream sidecar found; waiting for rollout"
	case len(failedPods) > 0:
		return false, fmt.Sprintf("Litestream sidecar unhealthy in pods: %v", failedPods)
	case runningCount > 0:
		return true, fmt.Sprintf("Litestream sidecar running in %d pod(s)", runningCount)
	default:
		return false, "Litestream sidecar not yet running"
	}
}

// isFailureReason returns true for container waiting reasons that indicate a
// non-transient failure (as opposed to normal startup states like
// ContainerCreating or PodInitializing).
func isFailureReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff", "OOMKilled", "Error", "ImagePullBackOff", "ErrImagePull":
		return true
	}
	return false
}

// updateStatus computes and writes the status subresource for the SQLiteDB,
// including sidecar injection state and backup health from live pod inspection.
func (r *SQLiteDBReconciler) updateStatus(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
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
		sqliteDB.Status.BackupHealthy = false
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionSidecarInjected,
			metav1.ConditionFalse, "DeploymentNotFound",
			fmt.Sprintf("target Deployment %q not found", sqliteDB.Spec.TargetDeployment),
			sqliteDB.Generation, now)
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionBackupHealthy,
			metav1.ConditionFalse, "DeploymentNotFound", "target Deployment not found",
			sqliteDB.Generation, now)
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReady,
			metav1.ConditionFalse, "DeploymentNotFound", "target Deployment not found",
			sqliteDB.Generation, now)
		return r.Status().Update(ctx, sqliteDB)
	}

	// --- SidecarInjected condition ---
	annotated := deployment.Spec.Template.Annotations[injectAnnotation] == "true"
	if annotated {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionSidecarInjected,
			metav1.ConditionTrue, "Annotated",
			"target Deployment is annotated for sidecar injection",
			sqliteDB.Generation, now)
	} else {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionSidecarInjected,
			metav1.ConditionFalse, "AnnotationPending",
			"injection annotation not yet applied to target Deployment",
			sqliteDB.Generation, now)
	}

	// --- BackupHealthy condition (pod inspection) ---
	prevHealthy := sqliteDB.Status.BackupHealthy
	if sqliteDB.Spec.Backup.Enabled {
		healthy, msg := r.litestreamContainerState(ctx, sqliteDB, deployment)
		sqliteDB.Status.BackupHealthy = healthy

		condStatus := metav1.ConditionFalse
		reason := "SidecarUnhealthy"
		if healthy {
			condStatus = metav1.ConditionTrue
			reason = "SidecarRunning"
		}
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionBackupHealthy,
			condStatus, reason, msg, sqliteDB.Generation, now)

		// Emit an event on transitions so kubectl describe shows a timeline.
		if healthy && !prevHealthy {
			r.Recorder.Event(sqliteDB, corev1.EventTypeNormal, "BackupHealthy",
				"Litestream sidecar is running and replicating")
		} else if !healthy && prevHealthy {
			r.Recorder.Event(sqliteDB, corev1.EventTypeWarning, "BackupUnhealthy", msg)
		}
	} else {
		sqliteDB.Status.BackupHealthy = false
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionBackupHealthy,
			metav1.ConditionFalse, "BackupDisabled",
			"backup is not enabled in spec",
			sqliteDB.Generation, now)
	}

	// --- Ready condition ---
	if annotated && deployment.Status.ReadyReplicas > 0 {
		sqliteDB.Status.Phase = databasev1.PhaseReady
		sqliteDB.Status.Ready = true
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReady,
			metav1.ConditionTrue, "DeploymentReady",
			"target Deployment has ready replicas",
			sqliteDB.Generation, now)
	} else {
		sqliteDB.Status.Phase = databasev1.PhasePending
		sqliteDB.Status.Ready = false
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReady,
			metav1.ConditionFalse, "DeploymentNotReady",
			"waiting for target Deployment to have ready replicas",
			sqliteDB.Generation, now)
	}

	return r.Status().Update(ctx, sqliteDB)
}

// setCondition is a thin wrapper around meta.SetStatusCondition that fills in
// ObservedGeneration and LastTransitionTime on every call.
func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, gen int64, now metav1.Time) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
		LastTransitionTime: now,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *SQLiteDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Map pod events back to the SQLiteDB that owns the pod's Deployment,
	// identified by the config annotation on the pod's labels.
	podToSQLiteDB := func(ctx context.Context, obj client.Object) []ctrl.Request {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return nil
		}
		ref := pod.Annotations[configAnnotation]
		if ref == "" {
			return nil
		}
		// ref is "namespace/name"
		ns, name := "", ref
		for i, c := range ref {
			if c == '/' {
				ns, name = ref[:i], ref[i+1:]
				break
			}
		}
		if ns == "" {
			ns = pod.Namespace
		}
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.SQLiteDB{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podToSQLiteDB)).
		Named("sqlitedb").
		Complete(r)
}
