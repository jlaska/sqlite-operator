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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// StorageSpec defines the storage configuration for SQLiteDB
type StorageSpec struct {
	// Size is the requested storage size for the database
	Size string `json:"size,omitempty"`

	// StorageClass is the name of the storage class to use for the database
	StorageClass string `json:"storageClass,omitempty"`
}

// SQLiteDBSpec defines the desired state of SQLiteDB.
type SQLiteDBSpec struct {
	// DatabaseName is the name of the SQLite database file
	DatabaseName string `json:"databaseName"`

	// Storage defines the storage configuration for the database
	Storage StorageSpec `json:"storage,omitempty"`

	// StorageSize is the requested storage size for the database (deprecated, use Storage.Size)
	StorageSize string `json:"storageSize,omitempty"`

	// InitSQL contains SQL statements to execute when creating the database
	InitSQL string `json:"initSQL,omitempty"`

	// Replicas defines the number of database replicas (for read scaling)
	Replicas *int32 `json:"replicas,omitempty"`

	// BackupEnabled enables automatic backups
	BackupEnabled bool `json:"backupEnabled,omitempty"`

	// BackupSchedule defines the backup schedule in cron format
	BackupSchedule string `json:"backupSchedule,omitempty"`
}

// SQLiteDBStatus defines the observed state of SQLiteDB.
type SQLiteDBStatus struct {
	// Phase represents the current phase of the SQLite database
	Phase string `json:"phase,omitempty"`

	// Ready indicates if the database is ready to accept connections
	Ready bool `json:"ready,omitempty"`

	// DatabaseSize shows the current database file size
	DatabaseSize string `json:"databaseSize,omitempty"`

	// LastBackup indicates when the last backup was performed
	LastBackup *metav1.Time `json:"lastBackup,omitempty"`

	// PodName is the name of the pod running the SQLite database
	PodName string `json:"podName,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SQLiteDB is the Schema for the sqlitedbs API.
type SQLiteDB struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SQLiteDBSpec   `json:"spec,omitempty"`
	Status SQLiteDBStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SQLiteDBList contains a list of SQLiteDB.
type SQLiteDBList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SQLiteDB `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SQLiteDB{}, &SQLiteDBList{})
}
