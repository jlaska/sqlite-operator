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
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

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
	injectAnnotation      = databasev1.AnnotationInject
	configAnnotation      = databasev1.AnnotationConfig
	pauseAnnotation       = databasev1.AnnotationPause
	skipArchiveAnnotation = databasev1.AnnotationSkipArchiveCheck
)

const (
	injectEnabled    = "true"
	pausedConfig     = "dbs: []\n"
	litestreamSidecar = "litestream"
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
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
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

	target := sqliteDB.Spec.TargetDeployment
	if sqliteDB.Spec.TargetStatefulSet != "" {
		target = sqliteDB.Spec.TargetStatefulSet
	}
	log.Info("Reconciling SQLiteDB", "target", target)

	if err := r.reconcileLitestreamConfig(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile Litestream ConfigMap")
		return ctrl.Result{}, err
	}

	if err := r.reconcileInitSQLConfig(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile init SQL ConfigMap")
		return ctrl.Result{}, err
	}

	if err := r.reconcileTargetAnnotation(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to annotate target workload")
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
// When the pause annotation is set, writes an empty dbs list so Litestream runs
// but replicates nothing — protecting the S3 backup chain during restores.
func (r *SQLiteDBReconciler) reconcileLitestreamConfig(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sqliteDB.Name + "-litestream",
			Namespace: sqliteDB.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		var config string
		if sqliteDB.Annotations[pauseAnnotation] == injectEnabled {
			config = pausedConfig
		} else {
			config = r.buildLitestreamConfig(sqliteDB)
		}
		cm.Data = map[string]string{
			"litestream.yml": config,
		}
		return controllerutil.SetControllerReference(sqliteDB, cm, r.Scheme)
	})

	return err
}

// buildLitestreamConfig renders the litestream.yml content for the given CR.
// Uses the singular `replica:` key (Litestream 0.5.x preferred form).
// This is a thin wrapper around the package-level buildLitestreamConfigYAML so
// the restore controller can call the same logic without a method receiver.
func (r *SQLiteDBReconciler) buildLitestreamConfig(sqliteDB *databasev1.SQLiteDB) string {
	return buildLitestreamConfigYAML(sqliteDB)
}

// buildLitestreamConfigYAML is the package-level implementation shared by both
// the SQLiteDB and SQLiteRestore controllers.
func buildLitestreamConfigYAML(sqliteDB *databasev1.SQLiteDB) string {
	dbPath := fmt.Sprintf("%s/%s", sqliteDB.Spec.DatabasePath, sqliteDB.Spec.DatabaseName)

	// Global settings: Prometheus metrics endpoint (upstream guide recommendation).
	cfg := "addr: \":9090\"\n"

	// Per-database config.
	cfg += fmt.Sprintf("dbs:\n  - path: %s\n", dbPath)

	if sqliteDB.Spec.Backup.Enabled && sqliteDB.Spec.Backup.Destination.S3 != nil {
		s3 := sqliteDB.Spec.Backup.Destination.S3
		cfg += "    replica:\n"
		cfg += "      type: s3\n"
		if s3.Endpoint != "" {
			// Ensure the endpoint has a scheme. Litestream defaults to HTTPS
			// when no scheme is present, which breaks plain-HTTP S3-compatible
			// stores like MinIO without TLS. Preserve any existing scheme.
			endpoint := s3.Endpoint
			if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
				endpoint = "http://" + endpoint
			}
			cfg += fmt.Sprintf("      endpoint: %s\n", endpoint)
			// MinIO and other S3-compatible stores require path-style addressing.
			cfg += "      force-path-style: true\n"
		}
		cfg += fmt.Sprintf("      bucket: %s\n", s3.Bucket)
		if s3.Path != "" {
			// Litestream 0.5.x appends "/L{N}/" to the configured path when
			// constructing S3 object keys. A trailing slash produces "//L0/"
			// which MinIO rejects as XMinioInvalidObjectName.
			cfg += fmt.Sprintf("      path: %s\n", strings.TrimRight(s3.Path, "/"))
		}
		if sqliteDB.Spec.Backup.Retention.Duration != "" {
			cfg += fmt.Sprintf("      retention: %s\n", sqliteDB.Spec.Backup.Retention.Duration)
		}
		if sqliteDB.Spec.Backup.SyncInterval != "" {
			cfg += fmt.Sprintf("      sync-interval: %s\n", sqliteDB.Spec.Backup.SyncInterval)
		}
	}

	return cfg
}

// initSQLHash returns the hex-encoded SHA-256 digest of the given SQL string.
func initSQLHash(sql string) string {
	h := sha256.Sum256([]byte(sql))
	return fmt.Sprintf("%x", h)
}

// reconcileInitSQLConfig creates or updates the ConfigMap that holds init.sql
// and records the content hash in the status. When InitSQL is empty, any
// existing ConfigMap is left in place (owned resources are GC'd on CR deletion).
func (r *SQLiteDBReconciler) reconcileInitSQLConfig(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	if sqliteDB.Spec.InitSQL == "" {
		// No init SQL — clear the hash from status if it was previously set.
		if sqliteDB.Status.InitSQLHash != "" {
			patch := client.MergeFrom(sqliteDB.DeepCopy())
			sqliteDB.Status.InitSQLHash = ""
			setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionInitSQLApplied,
				metav1.ConditionFalse, "NoInitSQL",
				"no initSQL configured",
				sqliteDB.Generation, metav1.Now())
			return r.Status().Patch(ctx, sqliteDB, patch)
		}
		return nil
	}

	hash := initSQLHash(sqliteDB.Spec.InitSQL)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sqliteDB.Name + "-init-sql",
			Namespace: sqliteDB.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"init.sql": sqliteDB.Spec.InitSQL,
		}
		return controllerutil.SetControllerReference(sqliteDB, cm, r.Scheme)
	})
	if err != nil {
		return err
	}

	// Update status hash and condition only when they differ to avoid
	// unnecessary status writes on every reconcile.
	if sqliteDB.Status.InitSQLHash == hash {
		return nil
	}

	patch := client.MergeFrom(sqliteDB.DeepCopy())
	sqliteDB.Status.InitSQLHash = hash
	setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionInitSQLApplied,
		metav1.ConditionTrue, "ConfigMapReady",
		fmt.Sprintf("init SQL ConfigMap ready (hash %.8s…)", hash),
		sqliteDB.Generation, metav1.Now())

	return r.Status().Patch(ctx, sqliteDB, patch)
}

// reconcileTargetAnnotation adds injection annotations to the target workload's
// pod template, triggering a rolling restart so new pods inherit them and the
// mutating webhook can inject the Litestream sidecar.
// If the target workload has more than one replica, the annotation is skipped
// and an event is emitted — Litestream requires exactly one writer.
func (r *SQLiteDBReconciler) reconcileTargetAnnotation(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	wt, err := r.getTargetWorkload(ctx, sqliteDB)
	if err != nil {
		return err
	}

	// Litestream requires exactly one writer. Emit a warning event and skip
	// annotation; updateStatus will set phase=Error with ConditionReplicaCountExceeded.
	if wt.desiredReplicas() > 1 {
		r.Recorder.Eventf(sqliteDB, corev1.EventTypeWarning, "ReplicaCountExceeded",
			"target %s %q has %d replicas; Litestream requires exactly 1 writer",
			wt.typeName(), wt.name(), wt.desiredReplicas())
		return nil
	}

	configRef := fmt.Sprintf("%s/%s", sqliteDB.Namespace, sqliteDB.Name)

	tmplAnnotations := wt.podTemplateAnnotations()
	tmplLabels := wt.podTemplateLabels()
	// Both annotation and label must be present: the annotation carries the
	// config ref (read by the webhook handler), and the label enables the
	// MutatingWebhookConfiguration's objectSelector to route the pod to the
	// webhook (Kubernetes objectSelector matches labels, not annotations).
	if tmplAnnotations[injectAnnotation] == injectEnabled &&
		tmplAnnotations[configAnnotation] == configRef &&
		tmplLabels[injectAnnotation] == injectEnabled {
		return nil
	}

	return r.patchWorkloadPodTemplate(ctx, wt,
		map[string]string{
			injectAnnotation: injectEnabled,
			configAnnotation: configRef,
		},
		map[string]string{
			injectAnnotation: injectEnabled,
		},
	)
}

// litestreamContainerState inspects pods belonging to the target workload
// and returns the state of the Litestream sidecar container across all
// running pods. It returns (healthy, message) where healthy is true only
// when at least one pod has the sidecar in a Running state and no pods have
// it in a terminal failure state (CrashLoopBackOff / OOMKilled / Error).
func (r *SQLiteDBReconciler) litestreamContainerState(ctx context.Context, sqliteDB *databasev1.SQLiteDB, wt *workloadTarget) (healthy bool, message string) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(sqliteDB.Namespace),
		client.MatchingLabels(wt.selectorLabels()),
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
			if cs.Name != litestreamSidecar {
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
	wt, err := r.getTargetWorkload(ctx, sqliteDB)

	patch := client.MergeFrom(sqliteDB.DeepCopy())
	now := metav1.Now()
	sqliteDB.Status.ObservedGeneration = sqliteDB.Generation

	if err != nil {
		sqliteDB.Status.Phase = databasev1.PhaseError
		sqliteDB.Status.Ready = false
		sqliteDB.Status.BackupHealthy = false
		notFoundReason := "WorkloadNotFound"
		notFoundMsg := fmt.Sprintf("target workload not found: %v", err)
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionSidecarInjected,
			metav1.ConditionFalse, notFoundReason, notFoundMsg,
			sqliteDB.Generation, now)
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionBackupHealthy,
			metav1.ConditionFalse, notFoundReason, notFoundMsg,
			sqliteDB.Generation, now)
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReady,
			metav1.ConditionFalse, notFoundReason, notFoundMsg,
			sqliteDB.Generation, now)
		return r.Status().Patch(ctx, sqliteDB, patch)
	}

	// --- Replica count guard: Litestream requires exactly one writer ---
	if wt.desiredReplicas() > 1 {
		sqliteDB.Status.Phase = databasev1.PhaseError
		sqliteDB.Status.Ready = false
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReplicaCountExceeded,
			metav1.ConditionTrue, "TooManyReplicas",
			fmt.Sprintf("target %s %q has %d replicas; Litestream requires exactly 1 writer",
				wt.typeName(), wt.name(), wt.desiredReplicas()),
			sqliteDB.Generation, now)
		return r.Status().Patch(ctx, sqliteDB, patch)
	}
	// Clear the condition when replicas are back to 1.
	meta.RemoveStatusCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReplicaCountExceeded)

	// --- ReplicationPaused condition ---
	isPaused := sqliteDB.Annotations[pauseAnnotation] == injectEnabled
	if isPaused {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReplicationPaused,
			metav1.ConditionTrue, "PauseAnnotationSet",
			"replication paused via annotation",
			sqliteDB.Generation, now)
	} else {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReplicationPaused,
			metav1.ConditionFalse, "ReplicationActive",
			"replication is active",
			sqliteDB.Generation, now)
	}

	// --- ArchiveCheckFailed condition (from archive-check init container status) ---
	if archiveCheckFailed, msg := r.archiveCheckState(ctx, sqliteDB, wt); archiveCheckFailed {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionArchiveCheckFailed,
			metav1.ConditionTrue, "ArchiveCheckFailed", msg,
			sqliteDB.Generation, now)
		sqliteDB.Status.Phase = databasev1.PhaseError
		sqliteDB.Status.Ready = false
		return r.Status().Patch(ctx, sqliteDB, patch)
	} else {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionArchiveCheckFailed,
			metav1.ConditionFalse, "ArchiveCheckPassed", msg,
			sqliteDB.Generation, now)
	}

	// --- SidecarInjected condition ---
	annotated := wt.podTemplateAnnotations()[injectAnnotation] == injectEnabled
	if annotated {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionSidecarInjected,
			metav1.ConditionTrue, "Annotated",
			fmt.Sprintf("target %s is annotated for sidecar injection", wt.typeName()),
			sqliteDB.Generation, now)
	} else {
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionSidecarInjected,
			metav1.ConditionFalse, "AnnotationPending",
			fmt.Sprintf("injection annotation not yet applied to target %s", wt.typeName()),
			sqliteDB.Generation, now)
	}

	// --- BackupHealthy condition (pod inspection) ---
	prevHealthy := sqliteDB.Status.BackupHealthy
	if sqliteDB.Spec.Backup.Enabled {
		healthy, msg := r.litestreamContainerState(ctx, sqliteDB, wt)
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

	// --- Paused phase (overrides Ready/Pending) ---
	if isPaused {
		sqliteDB.Status.Phase = databasev1.PhasePaused
		sqliteDB.Status.Ready = false
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReady,
			metav1.ConditionFalse, "ReplicationPaused",
			"replication is paused; waiting for pause annotation to be removed",
			sqliteDB.Generation, now)
		return r.Status().Patch(ctx, sqliteDB, patch)
	}

	// --- Ready condition ---
	if annotated && wt.readyReplicas() > 0 {
		sqliteDB.Status.Phase = databasev1.PhaseReady
		sqliteDB.Status.Ready = true
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReady,
			metav1.ConditionTrue, "WorkloadReady",
			fmt.Sprintf("target %s has ready replicas", wt.typeName()),
			sqliteDB.Generation, now)
	} else {
		sqliteDB.Status.Phase = databasev1.PhasePending
		sqliteDB.Status.Ready = false
		setCondition(&sqliteDB.Status.Conditions, databasev1.ConditionReady,
			metav1.ConditionFalse, "WorkloadNotReady",
			fmt.Sprintf("waiting for target %s to have ready replicas", wt.typeName()),
			sqliteDB.Generation, now)
	}

	return r.Status().Patch(ctx, sqliteDB, patch)
}

// archiveCheckState inspects pods to detect whether the archive-check init container
// has failed. Returns (failed, message). A failure means the DB is missing but S3
// has existing backup data — the pod is blocked until a SQLiteRestore resolves it.
func (r *SQLiteDBReconciler) archiveCheckState(ctx context.Context, sqliteDB *databasev1.SQLiteDB, wt *workloadTarget) (bool, string) {
	if !sqliteDB.Spec.Backup.Enabled {
		return false, "backup not enabled"
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(sqliteDB.Namespace),
		client.MatchingLabels(wt.selectorLabels()),
	); err != nil {
		return false, fmt.Sprintf("failed to list pods: %v", err)
	}

	const archiveCheckContainer = "litestream-archive-check"
	for i := range podList.Items {
		pod := &podList.Items[i]
		for _, cs := range pod.Status.InitContainerStatuses {
			if cs.Name != archiveCheckContainer {
				continue
			}
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
				return true, fmt.Sprintf(
					"archive check failed in pod %s: S3 has existing backup data but local database is missing; create a SQLiteRestore CR to recover",
					pod.Name,
				)
			}
		}
	}
	return false, "archive check passed"
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
