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

const defaultLitestreamImage = "litestream/litestream:0.5.14"

// SQLiteRestoreReconciler reconciles a SQLiteRestore object.
type SQLiteRestoreReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=database.example.com,resources=sqliterestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.example.com,resources=sqliterestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.example.com,resources=sqliterestores/finalizers,verbs=update
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

	log.Info("Reconciling SQLiteRestore", "source", restore.Spec.SourceRef)

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

	// Create or check the restore Job.
	job := &batchv1.Job{}
	jobName := restore.Name + "-restore"
	err := r.Get(ctx, types.NamespacedName{Namespace: restore.Namespace, Name: jobName}, job)

	switch {
	case errors.IsNotFound(err):
		// First reconcile — create the Job and move to Running.
		newJob := r.buildRestoreJob(restore, sourceDB, jobName)
		if err := controllerutil.SetControllerReference(restore, newJob, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, newJob); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating restore Job: %w", err)
		}
		log.Info("Created restore Job", "job", jobName)
		r.Recorder.Eventf(restore, corev1.EventTypeNormal, "RestoreStarted",
			"Created restore Job %s", jobName)
		now := metav1.Now()
		return ctrl.Result{}, r.setStatus(ctx, restore, databasev1.RestorePhaseRunning,
			jobName, "restore Job created", &now, nil)

	case err != nil:
		return ctrl.Result{}, fmt.Errorf("getting restore Job: %w", err)
	}

	// Job exists — sync its outcome into the restore status.
	return ctrl.Result{}, r.syncJobStatus(ctx, restore, job)
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
	}
	if restore.Spec.Timestamp != "" {
		args = append(args, "-timestamp", restore.Spec.Timestamp)
	}
	args = append(args, dbPathInConfig)

	mountPath := dirOf(restore.Spec.TargetPath)
	jobLabels := map[string]string{
		"app.kubernetes.io/managed-by":        "sqlite-operator",
		"sqlite.database.example.com/restore": restore.Name,
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
								{Name: "target", MountPath: mountPath},
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
							// Source DB's litestream.yml contains the S3 endpoint.
							Name: "litestream-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: sourceDB.Name + "-litestream",
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

// syncJobStatus maps Job conditions onto the SQLiteRestore status.
func (r *SQLiteRestoreReconciler) syncJobStatus(ctx context.Context, restore *databasev1.SQLiteRestore, job *batchv1.Job) error {
	for _, cond := range job.Status.Conditions {
		switch cond.Type {
		case batchv1.JobComplete:
			if cond.Status == corev1.ConditionTrue {
				r.Recorder.Event(restore, corev1.EventTypeNormal, "RestoreComplete",
					"Litestream restore Job completed successfully")
				var completionTime *metav1.Time
				if job.Status.CompletionTime != nil {
					t := metav1.Time{Time: job.Status.CompletionTime.Time}
					completionTime = &t
				}
				return r.setStatus(ctx, restore, databasev1.RestorePhaseComplete,
					job.Name, "restore completed successfully", restore.Status.StartTime, completionTime)
			}
		case batchv1.JobFailed:
			if cond.Status == corev1.ConditionTrue {
				msg := "restore Job failed"
				if cond.Message != "" {
					msg = cond.Message
				}
				r.Recorder.Event(restore, corev1.EventTypeWarning, "RestoreFailed", msg)
				var completionTime *metav1.Time
				if job.Status.CompletionTime != nil {
					t := metav1.Time{Time: job.Status.CompletionTime.Time}
					completionTime = &t
				}
				return r.setStatus(ctx, restore, databasev1.RestorePhaseFailed,
					job.Name, msg, restore.Status.StartTime, completionTime)
			}
		}
	}
	// Job still running — ensure phase is Running (idempotent).
	if restore.Status.Phase != databasev1.RestorePhaseRunning {
		return r.setStatus(ctx, restore, databasev1.RestorePhaseRunning,
			job.Name, "restore Job running", restore.Status.StartTime, nil)
	}
	return nil
}

// failRestore moves the restore to Failed with the given message.
func (r *SQLiteRestoreReconciler) failRestore(ctx context.Context, restore *databasev1.SQLiteRestore, msg string) error {
	r.Recorder.Event(restore, corev1.EventTypeWarning, "RestoreFailed", msg)
	now := metav1.Now()
	return r.setStatus(ctx, restore, databasev1.RestorePhaseFailed, "", msg, &now, &now)
}

// setStatus writes the given phase and metadata to the restore's status subresource.
func (r *SQLiteRestoreReconciler) setStatus(
	ctx context.Context,
	restore *databasev1.SQLiteRestore,
	phase, jobName, message string,
	startTime *metav1.Time,
	completionTime *metav1.Time,
) error {
	restore.Status.Phase = phase
	restore.Status.JobName = jobName
	restore.Status.Message = message
	if startTime != nil && restore.Status.StartTime == nil {
		restore.Status.StartTime = startTime
	}
	if completionTime != nil {
		restore.Status.CompletionTime = completionTime
	}
	return r.Status().Update(ctx, restore)
}

// dirOf returns the directory portion of a file path.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}

// SetupWithManager sets up the controller with the Manager.
func (r *SQLiteRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.SQLiteRestore{}).
		Owns(&batchv1.Job{}).
		Named("sqliterestore").
		Complete(r)
}
