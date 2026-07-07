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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

var _ = Describe("SQLiteRestore Controller", func() {
	const (
		restoreName   = "test-restore"
		sourceDBName  = "source-db"
		targetPVC     = "restore-pvc"
		targetPath    = "/data/myapp.db"
		secretRef     = "s3-creds"
		bucketName    = "my-backups"
		namespaceName = "default"
	)

	ctx := context.Background()
	restoreKey := types.NamespacedName{Name: restoreName, Namespace: namespaceName}
	sourceDBKey := types.NamespacedName{Name: sourceDBName, Namespace: namespaceName}

	newRestoreReconciler := func() *SQLiteRestoreReconciler {
		return &SQLiteRestoreReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(10),
		}
	}

	newSourceDB := func() *databasev1.SQLiteDB {
		return &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{Name: sourceDBName, Namespace: namespaceName},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     "myapp.db",
				DatabasePath:     "/data",
				TargetDeployment: "myapp",
				Backup: databasev1.BackupSpec{
					Enabled: true,
					Destination: databasev1.BackupDestination{
						S3: &databasev1.S3Destination{
							Bucket:    bucketName,
							Path:      "myapp/",
							SecretRef: secretRef,
						},
					},
				},
			},
		}
	}

	newRestore := func() *databasev1.SQLiteRestore {
		return &databasev1.SQLiteRestore{
			ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespaceName},
			Spec: databasev1.SQLiteRestoreSpec{
				SourceRef:  sourceDBName,
				TargetPVC:  targetPVC,
				TargetPath: targetPath,
			},
		}
	}

	BeforeEach(func() {
		db := &databasev1.SQLiteDB{}
		if err := k8sClient.Get(ctx, sourceDBKey, db); err != nil && errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, newSourceDB())).To(Succeed())
		}

		restore := &databasev1.SQLiteRestore{}
		if err := k8sClient.Get(ctx, restoreKey, restore); err != nil && errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, newRestore())).To(Succeed())
		}
	})

	AfterEach(func() {
		restore := &databasev1.SQLiteRestore{}
		if err := k8sClient.Get(ctx, restoreKey, restore); err == nil {
			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
		}

		db := &databasev1.SQLiteDB{}
		if err := k8sClient.Get(ctx, sourceDBKey, db); err == nil {
			Expect(k8sClient.Delete(ctx, db)).To(Succeed())
		}

		// Clean up the restore Job if it exists.
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: restoreName + "-restore", Namespace: namespaceName,
		}, job); err == nil {
			Expect(k8sClient.Delete(ctx, job)).To(Succeed())
		}
	})

	It("creates a restore Job with correct args and env vars", func() {
		_, err := newRestoreReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      restoreName + "-restore",
			Namespace: namespaceName,
		}, job)).To(Succeed())

		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Name).To(Equal("litestream-restore"))
		Expect(container.Image).To(ContainSubstring("litestream"))

		// Should use -config flag (endpoint comes from config file, not env var).
		Expect(container.Args).To(ContainElement("-config"))
		Expect(container.Args).To(ContainElement("/etc/litestream/litestream.yml"))
		// Should include -o <targetPath> and the db path from the source SQLiteDB spec.
		Expect(container.Args).To(ContainElement("-o"))
		Expect(container.Args).To(ContainElement(targetPath))

		// Should inject S3 credential env vars from the secret.
		envNames := make([]string, len(container.Env))
		for i, e := range container.Env {
			envNames[i] = e.Name
		}
		Expect(envNames).To(ContainElements("LITESTREAM_ACCESS_KEY_ID", "LITESTREAM_SECRET_ACCESS_KEY"))

		// Credential env vars must reference the correct secret.
		for _, e := range container.Env {
			if e.Name == "LITESTREAM_ACCESS_KEY_ID" {
				Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal(secretRef))
			}
		}
	})

	It("mounts the target PVC at the parent directory of TargetPath", func() {
		_, err := newRestoreReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      restoreName + "-restore",
			Namespace: namespaceName,
		}, job)).To(Succeed())

		volumes := job.Spec.Template.Spec.Volumes
		// Two volumes: target PVC + litestream-config ConfigMap.
		Expect(volumes).To(HaveLen(2))
		var pvcVol, cmVol corev1.Volume
		for _, v := range volumes {
			if v.PersistentVolumeClaim != nil {
				pvcVol = v
			}
			if v.ConfigMap != nil {
				cmVol = v
			}
		}
		Expect(pvcVol.PersistentVolumeClaim.ClaimName).To(Equal(targetPVC))
		Expect(cmVol.ConfigMap.Name).To(Equal(sourceDBName + "-litestream"))

		mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
		// Two mounts: target PVC at /data and litestream-config at /etc/litestream.
		Expect(mounts).To(HaveLen(2))
		mountPaths := make([]string, len(mounts))
		for i, m := range mounts {
			mountPaths[i] = m.MountPath
		}
		Expect(mountPaths).To(ContainElement("/data"))           // dirOf("/data/myapp.db")
		Expect(mountPaths).To(ContainElement("/etc/litestream")) // config file
	})

	It("includes -timestamp arg when PITR timestamp is set", func() {
		// Use a unique restore name to avoid sharing a Job with other specs.
		const pitrRestoreName = "pitr-restore"
		pitrKey := types.NamespacedName{Name: pitrRestoreName, Namespace: namespaceName}

		pitrRestore := &databasev1.SQLiteRestore{
			ObjectMeta: metav1.ObjectMeta{Name: pitrRestoreName, Namespace: namespaceName},
			Spec: databasev1.SQLiteRestoreSpec{
				SourceRef:  sourceDBName,
				TargetPVC:  targetPVC,
				TargetPath: targetPath,
				Timestamp:  "2026-06-17T10:00:00Z",
			},
		}
		Expect(k8sClient.Create(ctx, pitrRestore)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, pitrRestore)
			job := &batchv1.Job{}
			if err := k8sClient.Get(ctx, types.NamespacedName{
				Name: pitrRestoreName + "-restore", Namespace: namespaceName,
			}, job); err == nil {
				_ = k8sClient.Delete(ctx, job)
			}
		}()

		_, err := newRestoreReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: pitrKey})
		Expect(err).NotTo(HaveOccurred())

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      pitrRestoreName + "-restore",
			Namespace: namespaceName,
		}, job)).To(Succeed())

		args := job.Spec.Template.Spec.Containers[0].Args
		Expect(args).To(ContainElements("-timestamp", "2026-06-17T10:00:00Z"))
	})

	It("sets status to Running after creating the Job", func() {
		// Use a unique restore name to avoid interference from other specs' Jobs.
		const statusRestoreName = "status-restore"
		statusKey := types.NamespacedName{Name: statusRestoreName, Namespace: namespaceName}

		statusRestore := &databasev1.SQLiteRestore{
			ObjectMeta: metav1.ObjectMeta{Name: statusRestoreName, Namespace: namespaceName},
			Spec: databasev1.SQLiteRestoreSpec{
				SourceRef:  sourceDBName,
				TargetPVC:  targetPVC,
				TargetPath: targetPath,
			},
		}
		Expect(k8sClient.Create(ctx, statusRestore)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, statusRestore)
			job := &batchv1.Job{}
			if err := k8sClient.Get(ctx, types.NamespacedName{
				Name: statusRestoreName + "-restore", Namespace: namespaceName,
			}, job); err == nil {
				_ = k8sClient.Delete(ctx, job)
			}
		}()

		_, err := newRestoreReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: statusKey})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, statusKey, statusRestore)).To(Succeed())
		Expect(statusRestore.Status.Phase).To(Equal(databasev1.RestorePhaseRunning))
		Expect(statusRestore.Status.JobName).To(Equal(statusRestoreName + "-restore"))
		Expect(statusRestore.Status.StartTime).NotTo(BeNil())
	})

	It("is idempotent — does not create a second Job on re-reconcile", func() {
		reconciler := newRestoreReconciler()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Second reconcile — job already exists, should not error.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Still exactly one Job.
		jobList := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobList,
			client.InNamespace(namespaceName),
			client.MatchingLabels{"sqlite.database.example.com/restore": restoreName},
		)).To(Succeed())
		Expect(jobList.Items).To(HaveLen(1))
	})

	It("fails immediately when the referenced SQLiteDB has backup disabled", func() {
		// Create a separate restore referencing a DB with backup off.
		const badRestoreName = "bad-restore"
		noBackupDB := &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{Name: "no-backup-db", Namespace: namespaceName},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     "other.db",
				DatabasePath:     "/data",
				TargetDeployment: "other-app",
				Backup:           databasev1.BackupSpec{Enabled: false},
			},
		}
		Expect(k8sClient.Create(ctx, noBackupDB)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, noBackupDB) }()

		badRestore := &databasev1.SQLiteRestore{
			ObjectMeta: metav1.ObjectMeta{Name: badRestoreName, Namespace: namespaceName},
			Spec: databasev1.SQLiteRestoreSpec{
				SourceRef:  "no-backup-db",
				TargetPVC:  targetPVC,
				TargetPath: targetPath,
			},
		}
		Expect(k8sClient.Create(ctx, badRestore)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, badRestore) }()

		_, err := newRestoreReconciler().Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: badRestoreName, Namespace: namespaceName},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: badRestoreName, Namespace: namespaceName}, badRestore)).To(Succeed())
		Expect(badRestore.Status.Phase).To(Equal(databasev1.RestorePhaseFailed))
	})
})
