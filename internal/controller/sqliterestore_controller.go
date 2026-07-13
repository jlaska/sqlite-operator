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

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

const (
	defaultLitestreamImage  = databasev1.LitestreamDefaultImage
	restoreRequeueInterval  = 5 * time.Second
	restoreLabelKey         = "sqlite.database.example.com/restore"
	restoreTargetVolumeName = "target"
)

// SQLiteRestoreReconciler reconciles a SQLiteRestore object.
type SQLiteRestoreReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=database.example.com,resources=sqliterestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.example.com,resources=sqliterestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.example.com,resources=sqliterestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *SQLiteRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	restore := &databasev1.SQLiteRestore{}
	if err := r.Get(ctx, req.NamespacedName, restore); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Terminal phases — nothing more to do.
	if restore.Status.Phase == databasev1.RestorePhaseComplete ||
		restore.Status.Phase == databasev1.RestorePhaseFailed {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling SQLiteRestore", "phase", restore.Status.Phase, "source", restore.Spec.SourceRef)

	// Look up the referenced SQLiteDB to get backup config and credentials.
	sourceDB := &databasev1.SQLiteDB{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: restore.Namespace,
		Name:      restore.Spec.SourceRef,
	}, sourceDB); err != nil {
		return ctrl.Result{}, r.failRestore(ctx, restore,
			fmt.Sprintf("SQLiteDB %q not found: %v", restore.Spec.SourceRef, err))
	}

	if !sourceDB.Spec.Backup.Enabled || sourceDB.Spec.Backup.Destination.S3 == nil {
		return ctrl.Result{}, r.failRestore(ctx, restore,
			fmt.Sprintf("SQLiteDB %q does not have backup enabled with an S3 destination", restore.Spec.SourceRef))
	}

	switch restore.Status.Phase {
	case "", databasev1.RestorePhasePending:
		return r.reconcilePending(ctx, restore, sourceDB)
	case databasev1.RestorePhasePausing:
		return r.reconcilePausing(ctx, restore, sourceDB)
	case databasev1.RestorePhaseScalingDown:
		return r.reconcileScalingDown(ctx, restore, sourceDB)
	case databasev1.RestorePhaseRunning:
		return r.reconcileRunning(ctx, restore, sourceDB)
	case databasev1.RestorePhaseValidating:
		return r.reconcileValidating(ctx, restore, sourceDB)
	case databasev1.RestorePhaseScalingUp:
		return r.reconcileScalingUp(ctx, restore, sourceDB)
	default:
		return ctrl.Result{}, nil
	}
}

// reconcilePending handles the initial phase: record original replicas, set pause
// annotation on the SQLiteDB, transition to Pausing.
func (r *SQLiteRestoreReconciler) reconcilePending(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Record original replica count. If the workload is already gone (e.g. the
	// app was torn down before the restore), treat it as already at 0 — the
	// scale-down step will be skipped and the restore Job runs immediately.
	originalReplicas := int32(0)
	wt, err := r.getTargetWorkloadForRestore(ctx, sourceDB)
	switch {
	case err == nil:
		originalReplicas = wt.desiredReplicas()
	case errors.IsNotFound(err):
		log.Info("target workload not found; scale-down will be skipped")
	default:
		return ctrl.Result{}, r.failRestore(ctx, restore,
			fmt.Sprintf("getting target workload for SQLiteDB %q: %v", sourceDB.Name, err))
	}

	if err := r.pauseReplication(ctx, sourceDB); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting pause annotation on SQLiteDB: %w", err)
	}

	log.Info("Pausing replication before restore", "originalReplicas", originalReplicas)
	r.Recorder.Eventf(restore, corev1.EventTypeNormal, "PausingReplication",
		"Pausing Litestream replication on SQLiteDB %s", sourceDB.Name)

	now := metav1.Now()
	return ctrl.Result{RequeueAfter: restoreRequeueInterval}, r.setStatus(ctx, restore,
		databasev1.RestorePhasePausing, restore.Status.JobName, "pausing replication",
		&originalReplicas, &now, nil)
}

// reconcilePausing waits for the ConfigMap to reflect the pause (dbs: []) and then
// scales the Deployment to 0. The ConfigMap update is async (kubelet propagation),
// so we check the ConfigMap content before scaling.
func (r *SQLiteRestoreReconciler) reconcilePausing(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Verify pause annotation is still set (defensive — could have been removed).
	if sourceDB.Annotations[pauseAnnotation] != injectEnabled {
		if err := r.pauseReplication(ctx, sourceDB); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-setting pause annotation: %w", err)
		}
		return ctrl.Result{RequeueAfter: restoreRequeueInterval}, nil
	}

	// Verify ConfigMap reflects the pause (dbs: []).
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: sourceDB.Namespace,
		Name:      sourceDB.Name + "-litestream",
	}, cm); err != nil {
		return ctrl.Result{RequeueAfter: restoreRequeueInterval},
			fmt.Errorf("getting litestream ConfigMap: %w", err)
	}
	if cm.Data[litestreamConfigKey] != pausedConfig {
		log.Info("Waiting for ConfigMap to reflect pause")
		return ctrl.Result{RequeueAfter: restoreRequeueInterval}, nil
	}

	// Scale workload to 0. If it no longer exists, treat it as already scaled down.
	wt, err := r.getTargetWorkloadForRestore(ctx, sourceDB)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, r.failRestore(ctx, restore,
			fmt.Sprintf("cannot find target workload: %v", err))
	}
	if err == nil {
		if scaleErr := r.scaleWorkload(ctx, wt, 0); scaleErr != nil {
			return ctrl.Result{}, fmt.Errorf("scaling workload to 0: %w", scaleErr)
		}
		log.Info("Scaled workload to 0, waiting for pods to terminate")
		r.Recorder.Eventf(restore, corev1.EventTypeNormal, "ScalingDown",
			"Scaled %s %s to 0 replicas", wt.typeName(), wt.name())
	} else {
		log.Info("target workload not found during Pausing; proceeding without scale-down")
	}

	return ctrl.Result{RequeueAfter: restoreRequeueInterval}, r.setStatus(ctx, restore,
		databasev1.RestorePhaseScalingDown, restore.Status.JobName, "waiting for pods to terminate",
		restore.Status.OriginalReplicas, restore.Status.StartTime, nil)
}

// reconcileScalingDown polls until all pods have terminated, then creates the restore Job.
// If the workload no longer exists, treat it as already at 0 replicas.
func (r *SQLiteRestoreReconciler) reconcileScalingDown(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	wt, err := r.getTargetWorkloadForRestore(ctx, sourceDB)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, r.failRestore(ctx, restore,
			fmt.Sprintf("cannot find target workload: %v", err))
	}

	if err == nil && wt.runningReplicas() > 0 {
		log.Info("Waiting for workload pods to terminate", "currentReplicas", wt.runningReplicas())
		return ctrl.Result{RequeueAfter: restoreRequeueInterval}, nil
	}

	// All pods terminated — create a fresh litestream ConfigMap for the restore
	// job. The source SQLiteDB's ConfigMap is currently paused (dbs: []) so
	// the restore job needs its own copy with the actual S3 replica config.
	if err := r.reconcileRestoreConfig(ctx, restore, sourceDB); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating restore litestream config: %w", err)
	}

	// Create the restore Job.
	jobName := restore.Name + "-restore"
	newJob := r.buildRestoreJob(restore, sourceDB, jobName)
	if err := controllerutil.SetControllerReference(restore, newJob, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, newJob); err != nil {
		if !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("creating restore Job: %w", err)
		}
	}

	log.Info("Created restore Job", "job", jobName)
	r.Recorder.Eventf(restore, corev1.EventTypeNormal, "RestoreStarted",
		"Created restore Job %s", jobName)

	now := metav1.Now()
	return ctrl.Result{RequeueAfter: restoreRequeueInterval}, r.setStatus(ctx, restore,
		databasev1.RestorePhaseRunning, jobName, "restore Job running",
		restore.Status.OriginalReplicas, &now, nil)
}

// reconcileRunning watches the restore Job and transitions to ScalingUp on success
// or Failed (with cleanup) on failure.
func (r *SQLiteRestoreReconciler) reconcileRunning(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB) (ctrl.Result, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: restore.Namespace, Name: restore.Status.JobName}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			// Job disappeared — treat as failure.
			return ctrl.Result{}, r.failRestoreWithCleanup(ctx, restore, sourceDB, "restore Job not found")
		}
		return ctrl.Result{}, fmt.Errorf("getting restore Job: %w", err)
	}

	for _, cond := range job.Status.Conditions {
		switch cond.Type {
		case batchv1.JobComplete:
			if cond.Status == corev1.ConditionTrue {
				r.Recorder.Event(restore, corev1.EventTypeNormal, "RestoreJobComplete",
					"Restore Job completed; running integrity check")
				return ctrl.Result{RequeueAfter: restoreRequeueInterval}, r.setStatus(ctx, restore,
					databasev1.RestorePhaseValidating, restore.Status.JobName, "running integrity check",
					restore.Status.OriginalReplicas, restore.Status.StartTime, nil)
			}
		case batchv1.JobFailed:
			if cond.Status == corev1.ConditionTrue {
				msg := "restore Job failed"
				if cond.Message != "" {
					msg = cond.Message
				}
				return ctrl.Result{}, r.failRestoreWithCleanup(ctx, restore, sourceDB, msg)
			}
		}
	}

	// Job still running.
	return ctrl.Result{RequeueAfter: restoreRequeueInterval}, nil
}

// reconcileValidating creates a validation Job that runs PRAGMA quick_check on the
// restored database. On success it transitions to ScalingUp; on failure it calls
// failRestoreWithCleanup so the Deployment is scaled back up and replication resumed.
// This catches the restore corruption known in upstream issues #1164 and #1220 before
// the application restarts against a corrupt database.
func (r *SQLiteRestoreReconciler) reconcileValidating(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	validationJobName := restore.Name + "-validate"

	// Create the validation Job if it does not yet exist.
	validationJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: restore.Namespace, Name: validationJobName}, validationJob)
	if errors.IsNotFound(err) {
		newJob := r.buildValidationJob(restore, sourceDB, validationJobName)
		if setErr := controllerutil.SetControllerReference(restore, newJob, r.Scheme); setErr != nil {
			return ctrl.Result{}, setErr
		}
		if createErr := r.Create(ctx, newJob); createErr != nil && !errors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, fmt.Errorf("creating validation Job: %w", createErr)
		}
		log.Info("Created integrity-check validation Job", "job", validationJobName)
		r.Recorder.Eventf(restore, corev1.EventTypeNormal, "ValidationStarted",
			"Running PRAGMA quick_check on restored database")
		return ctrl.Result{RequeueAfter: restoreRequeueInterval}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting validation Job: %w", err)
	}

	for _, cond := range validationJob.Status.Conditions {
		switch cond.Type {
		case batchv1.JobComplete:
			if cond.Status == corev1.ConditionTrue {
				r.Recorder.Event(restore, corev1.EventTypeNormal, "ValidationPassed",
					"Integrity check passed; scaling Deployment back up")
				return ctrl.Result{RequeueAfter: restoreRequeueInterval}, r.setStatus(ctx, restore,
					databasev1.RestorePhaseScalingUp, restore.Status.JobName, "scaling Deployment back up",
					restore.Status.OriginalReplicas, restore.Status.StartTime, nil)
			}
		case batchv1.JobFailed:
			if cond.Status == corev1.ConditionTrue {
				msg := "integrity check failed on restored database: PRAGMA quick_check returned errors. " +
					"The S3 backup may be corrupt (Litestream upstream #1164/#1220). " +
					"Try creating a new SQLiteRestore CR with an earlier -timestamp."
				return ctrl.Result{}, r.failRestoreWithCleanup(ctx, restore, sourceDB, msg)
			}
		}
	}

	// Validation Job still running.
	return ctrl.Result{RequeueAfter: restoreRequeueInterval}, nil
}

// buildValidationJob constructs a Job that runs `sqlite3 <targetPath> "PRAGMA quick_check;"`.
// It uses the initImage from the source SQLiteDB (keinos/sqlite3 by default) which
// includes the sqlite3 CLI. The job mounts the target PVC at the parent directory.
func (r *SQLiteRestoreReconciler) buildValidationJob(
	restore *databasev1.SQLiteRestore,
	sourceDB *databasev1.SQLiteDB,
	jobName string,
) *batchv1.Job {
	image := sourceDB.Spec.InitImage
	if image == "" {
		image = "keinos/sqlite3:latest"
	}

	mountPath := filepath.Dir(restore.Spec.TargetPath)
	jobLabels := map[string]string{
		"app.kubernetes.io/managed-by":        "sqlite-operator",
		restoreLabelKey: restore.Name,
	}

	// Run PRAGMA quick_check; output must be exactly "ok" for the job to succeed.
	script := fmt.Sprintf(
		`result=$(sqlite3 %q "PRAGMA quick_check;"); if [ "$result" = "ok" ]; then echo "ok"; exit 0; fi; echo "integrity check failed: $result"; exit 1`,
		restore.Spec.TargetPath,
	)

	backoffLimit := int32(1)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: restore.Namespace,
			Labels:    jobLabels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "sqlite-integrity-check",
							Image:   image,
							Command: []string{"sh", "-c", script},
							VolumeMounts: []corev1.VolumeMount{
								{Name: restoreTargetVolumeName, MountPath: mountPath},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "target",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: restore.Spec.TargetPVC,
								},
							},
						},
					},
				},
			},
		},
	}
}

// reconcileScalingUp scales the workload back to its original replica count,
// removes the pause annotation, and transitions to Complete.
func (r *SQLiteRestoreReconciler) reconcileScalingUp(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	target := int32(1)
	if restore.Status.OriginalReplicas != nil {
		target = *restore.Status.OriginalReplicas
	}
	if target > 0 {
		wt, err := r.getTargetWorkloadForRestore(ctx, sourceDB)
		if err != nil && !errors.IsNotFound(err) {
			log.Error(err, "Cannot find target workload during scale-up; removing pause anyway")
		} else if err == nil {
			if scaleErr := r.scaleWorkload(ctx, wt, target); scaleErr != nil {
				return ctrl.Result{}, fmt.Errorf("scaling workload back to %d: %w", target, scaleErr)
			}
			log.Info("Scaled workload back up", "replicas", target)
		}
	}

	if err := r.resumeReplication(ctx, sourceDB); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing pause annotation from SQLiteDB: %w", err)
	}

	r.Recorder.Event(restore, corev1.EventTypeNormal, "RestoreComplete",
		"Litestream restore completed successfully")

	now := metav1.Now()
	return ctrl.Result{}, r.setStatus(ctx, restore,
		databasev1.RestorePhaseComplete, restore.Status.JobName, "restore completed successfully",
		restore.Status.OriginalReplicas, restore.Status.StartTime, &now)
}

// failRestoreWithCleanup transitions to Failed and attempts to restore the Deployment
// to its original replica count and remove the pause annotation.
func (r *SQLiteRestoreReconciler) failRestoreWithCleanup(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB, msg string) error {
	log := logf.FromContext(ctx)

	// Best-effort cleanup: scale back up if still at 0 and the workload exists.
	if wt, err := r.getTargetWorkloadForRestore(ctx, sourceDB); err == nil && wt.runningReplicas() == 0 {
		target := int32(1)
		if restore.Status.OriginalReplicas != nil {
			target = *restore.Status.OriginalReplicas
		}
		if target > 0 {
			if scaleErr := r.scaleWorkload(ctx, wt, target); scaleErr != nil {
				log.Error(scaleErr, "Failed to scale workload back up during cleanup")
			}
		}
	}

	// Best-effort cleanup: remove pause annotation.
	if resumeErr := r.resumeReplication(ctx, sourceDB); resumeErr != nil {
		log.Error(resumeErr, "Failed to remove pause annotation during cleanup")
	}

	r.Recorder.Event(restore, corev1.EventTypeWarning, "RestoreFailed", msg)
	now := metav1.Now()
	return r.setStatus(ctx, restore, databasev1.RestorePhaseFailed,
		restore.Status.JobName, msg, restore.Status.OriginalReplicas, restore.Status.StartTime, &now)
}

// pauseReplication sets the pause annotation on the SQLiteDB CR.
func (r *SQLiteRestoreReconciler) pauseReplication(ctx context.Context, db *databasev1.SQLiteDB) error {
	if db.Annotations[pauseAnnotation] == injectEnabled {
		return nil // already paused
	}
	patch := client.MergeFrom(db.DeepCopy())
	if db.Annotations == nil {
		db.Annotations = map[string]string{}
	}
	db.Annotations[pauseAnnotation] = injectEnabled
	return r.Patch(ctx, db, patch)
}

// resumeReplication removes the pause annotation from the SQLiteDB CR.
func (r *SQLiteRestoreReconciler) resumeReplication(ctx context.Context, db *databasev1.SQLiteDB) error {
	if db.Annotations[pauseAnnotation] != injectEnabled {
		return nil // not paused
	}
	// Re-fetch to get the latest version before patching.
	latest := &databasev1.SQLiteDB{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: db.Namespace, Name: db.Name}, latest); err != nil {
		return err
	}
	patch := client.MergeFrom(latest.DeepCopy())
	delete(latest.Annotations, pauseAnnotation)
	return r.Patch(ctx, latest, patch)
}

// buildRestoreJob constructs the Job that runs `litestream restore`.
func (r *SQLiteRestoreReconciler) buildRestoreJob(
	restore *databasev1.SQLiteRestore,
	sourceDB *databasev1.SQLiteDB,
	jobName string,
) *batchv1.Job {
	s3 := sourceDB.Spec.Backup.Destination.S3

	image := restore.Spec.Image
	if image == "" {
		image = sourceDB.Spec.Image
	}
	if image == "" {
		image = defaultLitestreamImage
	}

	// litestream restore -config /etc/litestream/litestream.yml \
	//                    [-timestamp <ts>]                        \
	//                    -o <targetPath>                          \
	//                    <dbPathInConfig>
	//
	// The db path must match the 'dbs[].path' key in litestream.yml so
	// Litestream can look up the replica config (endpoint, bucket, path).
	// Litestream has no env-var equivalent for the S3 endpoint — it must
	// come from the config file.
	dbPathInConfig := sourceDB.Spec.DatabasePath + "/" + sourceDB.Spec.DatabaseName
	args := []string{
		"restore",
		"-config", "/etc/litestream/litestream.yml",
		"-o", restore.Spec.TargetPath,
		// Note: Litestream 0.5.x does not have a -force flag. If a partial output
		// file exists from a previous failed attempt, Litestream overwrites it
		// because it constructs the output from LTX snapshots from scratch.
	}
	if restore.Spec.Timestamp != "" {
		args = append(args, "-timestamp", restore.Spec.Timestamp)
	}
	args = append(args, dbPathInConfig)

	mountPath := filepath.Dir(restore.Spec.TargetPath)
	jobLabels := map[string]string{
		"app.kubernetes.io/managed-by":        "sqlite-operator",
		restoreLabelKey: restore.Name,
	}

	backoffLimit := int32(3)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: restore.Namespace,
			Labels:    jobLabels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:  "litestream-restore",
							Image: image,
							Args:  args,
							// Credentials via env vars (supported by Litestream).
							// Endpoint is read from the mounted litestream.yml config.
							Env: s3EnvVars(s3.SecretRef),
							VolumeMounts: []corev1.VolumeMount{
								{Name: restoreTargetVolumeName, MountPath: mountPath},
								{Name: "litestream-config", MountPath: "/etc/litestream", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "target",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: restore.Spec.TargetPVC,
								},
							},
						},
						{
							// Dedicated restore config with full S3 replica settings.
							// The source SQLiteDB's ConfigMap is paused (dbs: []) during
							// restore, so we use a fresh copy named after the restore CR.
							Name: "litestream-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: restore.Name + "-litestream",
									},
								},
							},
						},
					},
				},
			},
		},
	}
	return job
}

// reconcileRestoreConfig creates a dedicated litestream ConfigMap for the restore
// job. The source SQLiteDB's ConfigMap is paused (dbs: []) while a restore is in
// progress, so the restore job needs its own copy of the full S3 replica config.
func (r *SQLiteRestoreReconciler) reconcileRestoreConfig(ctx context.Context, restore *databasev1.SQLiteRestore, sourceDB *databasev1.SQLiteDB) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restore.Name + "-litestream",
			Namespace: restore.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			litestreamConfigKey: buildLitestreamConfigYAML(sourceDB),
		}
		return controllerutil.SetControllerReference(restore, cm, r.Scheme)
	})
	return err
}

// s3EnvVars returns S3 credential env vars sourced from the named Secret.
// Litestream supports LITESTREAM_ACCESS_KEY_ID / LITESTREAM_SECRET_ACCESS_KEY
// as fallbacks for AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
// The S3 endpoint is NOT settable via env var in Litestream — it must come
// from the litestream.yml config file (mounted via buildRestoreJob).
func s3EnvVars(secretRef string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name: "LITESTREAM_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretRef},
					Key:                  "ACCESS_KEY_ID",
				},
			},
		},
		{
			Name: "LITESTREAM_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretRef},
					Key:                  "SECRET_ACCESS_KEY",
				},
			},
		},
	}
}

// failRestore moves the restore to Failed with the given message.
func (r *SQLiteRestoreReconciler) failRestore(ctx context.Context, restore *databasev1.SQLiteRestore, msg string) error {
	r.Recorder.Event(restore, corev1.EventTypeWarning, "RestoreFailed", msg)
	now := metav1.Now()
	return r.setStatus(ctx, restore, databasev1.RestorePhaseFailed, "",
		msg, nil, &now, &now)
}

// setStatus writes the given phase and metadata to the restore's status subresource.
func (r *SQLiteRestoreReconciler) setStatus(
	ctx context.Context,
	restore *databasev1.SQLiteRestore,
	phase, jobName, message string,
	originalReplicas *int32,
	startTime *metav1.Time,
	completionTime *metav1.Time,
) error {
	patch := client.MergeFrom(restore.DeepCopy())
	restore.Status.Phase = phase
	if jobName != "" {
		restore.Status.JobName = jobName
	}
	restore.Status.Message = message
	if originalReplicas != nil && restore.Status.OriginalReplicas == nil {
		restore.Status.OriginalReplicas = originalReplicas
	}
	if startTime != nil && restore.Status.StartTime == nil {
		restore.Status.StartTime = startTime
	}
	if completionTime != nil {
		restore.Status.CompletionTime = completionTime
	}
	return r.Status().Patch(ctx, restore, patch)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SQLiteRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.SQLiteRestore{}).
		Owns(&batchv1.Job{}).
		Named("sqliterestore").
		Complete(r)
}
