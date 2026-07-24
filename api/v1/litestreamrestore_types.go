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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RestorePhase constants for LitestreamRestoreStatus.Phase.
const (
	RestorePhasePending     = "Pending"
	RestorePhasePausing     = "Pausing"     // pause annotation set, waiting for ConfigMap propagation
	RestorePhaseScalingDown = "ScalingDown" // Deployment scaled to 0, waiting for pods to terminate
	RestorePhaseRunning     = "Running"
	RestorePhaseValidating  = "Validating" // restore Job done; running PRAGMA quick_check integrity check
	RestorePhaseScalingUp   = "ScalingUp"  // restore validated, scaling Deployment back up
	RestorePhaseComplete    = "Complete"
	RestorePhaseFailed      = "Failed"
)

// LitestreamRestoreSpec defines the desired state of a LitestreamRestore operation.
type LitestreamRestoreSpec struct {
	// SourceRef is the name of the LitestreamReplica CR whose backup configuration
	// (S3 destination + credentials) should be used as the restore source.
	// The LitestreamReplica must be in the same namespace.
	// +kubebuilder:validation:Required
	SourceRef string `json:"sourceRef"`

	// TargetPVC is the name of the PersistentVolumeClaim that the restored
	// database file will be written into. The PVC must already exist.
	// +kubebuilder:validation:Required
	TargetPVC string `json:"targetPVC"`

	// TargetPath is the full path (including filename) where the restored
	// database file will be written inside the TargetPVC.
	// Example: "/data/paperless.db"
	// +kubebuilder:validation:Required
	TargetPath string `json:"targetPath"`

	// Timestamp enables point-in-time recovery. When set, Litestream restores
	// the database to the state it was in at this instant.
	// Format: RFC 3339, e.g. "2026-06-17T10:00:00Z".
	// When omitted the most recent snapshot is restored.
	Timestamp string `json:"timestamp,omitempty"`

	// Image overrides the Litestream container image used for the restore Job.
	// Defaults to the image specified in the referenced LitestreamReplica, or
	// litestream/litestream:0.5.14 if neither is set.
	Image string `json:"image,omitempty"`

	// Force causes the restore Job to pass -force to litestream, overwriting an
	// existing database file at TargetPath. By default, litestream refuses to
	// overwrite a non-empty file. Set this to true only when you intentionally
	// want to replace an existing database (e.g. recovering from a diverged DB).
	// +optional
	Force bool `json:"force,omitempty"`

	// RunAsUser sets the UID for both the restore Job and the validation Job pods.
	// When omitted, the container image's default user is used (root for the standard
	// litestream image). Set this to match your application's UID (e.g. 1000) so that
	// the restored file is readable by non-root containers and to satisfy PSA Restricted
	// namespaces which reject runAsUser: 0.
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// RunAsGroup sets the GID for both the restore Job and the validation Job pods.
	// When omitted, the container image's default group is used.
	// +optional
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
}

// LitestreamRestoreStatus defines the observed state of a LitestreamRestore operation.
type LitestreamRestoreStatus struct {
	// Phase is the current lifecycle state.
	Phase string `json:"phase,omitempty"`

	// JobName is the name of the Kubernetes Job created to perform the restore.
	JobName string `json:"jobName,omitempty"`

	// OriginalReplicas records the target Deployment's replica count before
	// scale-down so it can be restored after the restore Job completes.
	OriginalReplicas *int32 `json:"originalReplicas,omitempty"`

	// StartTime is when the restore Job was created.
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the restore Job finished (successfully or not).
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message contains a human-readable description of the current phase,
	// including any error details on failure.
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=".spec.sourceRef"
// +kubebuilder:printcolumn:name="TargetPVC",type=string,JSONPath=".spec.targetPVC"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LitestreamRestore triggers a one-shot restore of a SQLite database from a
// Litestream backup stored in S3-compatible object storage.
type LitestreamRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LitestreamRestoreSpec   `json:"spec,omitempty"`
	Status LitestreamRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LitestreamRestoreList contains a list of LitestreamRestore.
type LitestreamRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LitestreamRestore `json:"items"`
}
