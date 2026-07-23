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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// S3Destination defines an S3-compatible backup destination.
type S3Destination struct {
	// Endpoint is the S3-compatible endpoint URL (e.g. "minio.homelab:9000").
	// Leave empty for AWS S3.
	Endpoint string `json:"endpoint,omitempty"`

	// Bucket is the name of the S3 bucket.
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// Path is the key prefix within the bucket (e.g. "paperless/").
	Path string `json:"path,omitempty"`

	// SecretRef names a Secret in the same namespace containing S3 credentials.
	// The Secret must have keys: ACCESS_KEY_ID, SECRET_ACCESS_KEY.
	// +kubebuilder:validation:Required
	SecretRef string `json:"secretRef"`
}

// BackupDestination selects which backup backend to use.
// Exactly one field must be set.
type BackupDestination struct {
	// S3 configures an S3-compatible backup destination.
	S3 *S3Destination `json:"s3,omitempty"`
}

// RetentionPolicy defines how long Litestream retains backup data.
type RetentionPolicy struct {
	// Duration is how long to retain backup data, expressed as a Go duration
	// string (e.g. "720h" for 30 days, "168h" for 7 days).
	// Litestream 0.5.x uses duration-based retention.
	// +kubebuilder:default="720h"
	Duration string `json:"duration,omitempty"`
}

// BackupSpec defines the Litestream backup configuration.
type BackupSpec struct {
	// Enabled controls whether Litestream replication is active.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// Destination specifies where backups are stored.
	Destination BackupDestination `json:"destination,omitempty"`

	// Retention controls how long backup data is kept.
	Retention RetentionPolicy `json:"retention,omitempty"`

	// SyncInterval overrides the Litestream sync-interval config key, controlling
	// how frequently WAL changes are synced to the replica. Expressed as a Go
	// duration string (e.g. "1s", "500ms"). When empty, the Litestream default (1s)
	// is used and the key is omitted from the generated config.
	SyncInterval string `json:"syncInterval,omitempty"`

	// LogLevel sets the Litestream logging verbosity via the LITESTREAM_LOG_LEVEL
	// environment variable. Valid values: "debug", "info", "warn", "error".
	// When empty, Litestream's default ("info") is used.
	// +kubebuilder:validation:Enum=debug;info;warn;error;""
	LogLevel string `json:"logLevel,omitempty"`

	// Resources specifies the compute resource requirements for the Litestream
	// sidecar container. When omitted, a default ephemeral-storage limit is applied
	// to guard against silent disk-fill (upstream issue #1310).
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// AutoRestore, when true, replaces the archive-check init container with an
	// upstream-style restore init container that automatically restores the database
	// from S3 on pod startup (if the local DB is missing and a backup exists).
	// A PRAGMA quick_check integrity gate is always run after restore; if it fails,
	// the pod is blocked from starting.
	//
	// WARNING: Litestream has known restore corruption issues (#1164, #1220).
	// The integrity gate catches corruption before the app starts. When false
	// (the default), pod startup is blocked if S3 has data but the local DB is
	// missing, requiring explicit recovery via a LitestreamRestore CR.
	// +kubebuilder:default=false
	AutoRestore bool `json:"autoRestore,omitempty"`
}

// LitestreamReplicaSpec defines the desired state of LitestreamReplica.
type LitestreamReplicaSpec struct {
	// DatabaseName is the filename of the SQLite database (e.g. "paperless.db").
	// +kubebuilder:validation:Required
	DatabaseName string `json:"databaseName"`

	// DatabasePath is the directory path inside the application container where
	// the database file lives (e.g. "/data"). The Litestream sidecar will be
	// configured to watch DatabasePath/DatabaseName.
	// +kubebuilder:validation:Required
	DatabasePath string `json:"databasePath"`

	// TargetDeployment is the name of the existing Deployment in this namespace
	// that the Litestream sidecar should be injected into.
	// Mutually exclusive with TargetStatefulSet.
	TargetDeployment string `json:"targetDeployment,omitempty"`

	// TargetStatefulSet is the name of an existing StatefulSet in this namespace
	// that the Litestream sidecar should be injected into.
	// Mutually exclusive with TargetDeployment.
	TargetStatefulSet string `json:"targetStatefulSet,omitempty"`

	// Image overrides the Litestream container image used for the sidecar.
	// +kubebuilder:default="litestream/litestream:0.5.14"
	Image string `json:"image,omitempty"`

	// Backup defines the Litestream replication / backup configuration.
	Backup BackupSpec `json:"backup,omitempty"`

	// InitSQL contains one or more SQL statements to execute against the
	// database on first use. The operator tracks a SHA-256 hash of this
	// content; the statements are (re-)applied only when the hash changes,
	// making updates idempotent across pod restarts.
	// Use IF NOT EXISTS guards to make individual statements safe to re-run.
	InitSQL string `json:"initSQL,omitempty"`

	// InitImage is the container image used for the init container that
	// applies InitSQL. Must include the sqlite3 CLI.
	// +kubebuilder:default="keinos/sqlite3:latest"
	InitImage string `json:"initImage,omitempty"`

	// RunAsUser sets the UID for Litestream-managed init containers injected by the
	// webhook (archive-check, auto-restore, db-init). When set, restored database files
	// are owned by this UID, allowing non-root application containers to read them.
	// When omitted, the container image's default user (root for litestream) is used.
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// RunAsGroup sets the GID for Litestream-managed init containers injected by the webhook.
	// When omitted, the container image's default group is used.
	// +optional
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
}

// Annotation keys placed on a Deployment's pod template by the controller.
// Pods inherit these annotations, signalling the mutating webhook to inject
// the Litestream sidecar.
const (
	// AnnotationInject signals that the Litestream sidecar should be injected.
	AnnotationInject = "litestream.io/inject"

	// AnnotationConfig records the "namespace/name" reference to the LitestreamReplica CR
	// that configures the sidecar for a given pod.
	AnnotationConfig = "litestream.io/config"

	// AnnotationPause, when set to "true" on a LitestreamReplica CR, causes the controller
	// to write an empty dbs list to the Litestream ConfigMap, pausing replication
	// without killing the sidecar process. Used by the restore controller during
	// coordinated restores and available for manual operational use.
	AnnotationPause = "litestream.io/pause"

	// AnnotationSkipArchiveCheck, when set to "true" on a LitestreamReplica CR, disables
	// the archive-check init container injected by the webhook. Use when
	// intentionally starting fresh against an existing S3 backup chain.
	AnnotationSkipArchiveCheck = "litestream.io/skip-archive-check"
)

// Condition type constants.
const (
	// ConditionSidecarInjected indicates the Litestream sidecar has been
	// injected into the target Deployment's pod template.
	ConditionSidecarInjected = "SidecarInjected"

	// ConditionBackupHealthy indicates the most recent backup succeeded.
	ConditionBackupHealthy = "BackupHealthy"

	// ConditionInitSQLApplied indicates the InitSQL has been configured and
	// the init container is ready to apply it on next pod start.
	ConditionInitSQLApplied = "InitSQLApplied"

	// ConditionReplicationPaused indicates that Litestream replication has been
	// intentionally paused via the AnnotationPause annotation.
	ConditionReplicationPaused = "ReplicationPaused"

	// ConditionArchiveCheckFailed indicates that the archive-check init container
	// detected a mismatch: the local DB is missing but S3 has existing backup data.
	// The pod is blocked from starting until a LitestreamRestore resolves the state.
	ConditionArchiveCheckFailed = "ArchiveCheckFailed"

	// ConditionReplicaCountExceeded indicates that the target workload has more
	// than one replica. Litestream requires exactly one writer to avoid database
	// corruption from concurrent writes.
	ConditionReplicaCountExceeded = "ReplicaCountExceeded"

	// ConditionReady is the top-level readiness condition.
	ConditionReady = "Ready"
)

// Phase constants for LitestreamReplicaStatus.Phase.
const (
	PhaseConfiguring = "Configuring"
	PhasePending     = "Pending"
	PhaseReady       = "Ready"
	PhasePaused      = "Paused"
	PhaseError       = "Error"
)

// LitestreamReplicaStatus defines the observed state of LitestreamReplica.
type LitestreamReplicaStatus struct {
	// Phase is the high-level lifecycle state: Configuring, Pending, Ready, Error.
	Phase string `json:"phase,omitempty"`

	// Ready mirrors the Ready condition status for quick kubectl output.
	Ready bool `json:"ready,omitempty"`

	// BackupHealthy indicates the last backup/replication check succeeded.
	BackupHealthy bool `json:"backupHealthy,omitempty"`

	// LastBackup is the timestamp of the most recent successful backup.
	LastBackup *metav1.Time `json:"lastBackup,omitempty"`

	// ReplicationLag is the approximate lag reported by Litestream (human-readable).
	ReplicationLag string `json:"replicationLag,omitempty"`

	// InitSQLHash is the SHA-256 hash of the InitSQL currently configured in
	// the spec. The init container uses this to name its marker file, so a
	// hash change triggers re-application on next pod rollout.
	InitSQLHash string `json:"initSQLHash,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds standard Kubernetes condition entries.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.targetDeployment"
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=".spec.databaseName"
// +kubebuilder:printcolumn:name="Backup",type=boolean,JSONPath=".spec.backup.enabled"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LitestreamReplica is the Schema for the litestreamreplicas API.
type LitestreamReplica struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LitestreamReplicaSpec   `json:"spec,omitempty"`
	Status LitestreamReplicaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LitestreamReplicaList contains a list of LitestreamReplica.
type LitestreamReplicaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LitestreamReplica `json:"items"`
}
