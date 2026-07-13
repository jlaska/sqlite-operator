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

// RestorePhase constants for SQLiteRestoreStatus.Phase.
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

// SQLiteRestoreSpec defines the desired state of a SQLiteRestore operation.
type SQLiteRestoreSpec struct {
	// SourceRef is the name of the SQLiteDB CR whose backup configuration
	// (S3 destination + credentials) should be used as the restore source.
	// The SQLiteDB must be in the same namespace.
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
	// Defaults to the image specified in the referenced SQLiteDB, or
	// litestream/litestream:0.5.14 if neither is set.
	Image string `json:"image,omitempty"`
}

// SQLiteRestoreStatus defines the observed state of a SQLiteRestore operation.
type SQLiteRestoreStatus struct {
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

// SQLiteRestore triggers a one-shot restore of a SQLite database from a
// Litestream backup stored in S3-compatible object storage.
type SQLiteRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SQLiteRestoreSpec   `json:"spec,omitempty"`
	Status SQLiteRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SQLiteRestoreList contains a list of SQLiteRestore.
type SQLiteRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SQLiteRestore `json:"items"`
}
