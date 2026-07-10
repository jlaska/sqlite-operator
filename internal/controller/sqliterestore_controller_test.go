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
	appsv1 "k8s.io/api/apps/v1"
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
		sourceDepName = "myapp"
		targetPVC     = "restore-pvc"
		targetPath    = "/data/myapp.db"
		secretRef     = "s3-creds"
		bucketName    = "my-backups"
		namespaceName = "default"
	)

	ctx := context.Background()
	restoreKey := types.NamespacedName{Name: restoreName, Namespace: namespaceName}
	sourceDBKey := types.NamespacedName{Name: sourceDBName, Namespace: namespaceName}
	sourceDepKey := types.NamespacedName{Name: sourceDepName, Namespace: namespaceName}
	sourceConfigMapKey := types.NamespacedName{Name: sourceDBName + "-litestream", Namespace: namespaceName}

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
				TargetDeployment: sourceDepName,
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

	// positionAtScalingDown pre-positions the given restore at ScalingDown phase with
	// the Deployment status.replicas=0, so the next reconcile creates the restore Job.
	positionAtScalingDown := func(rKey types.NamespacedName) {
		replicas := int32(1)
		r := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, rKey, r)).To(Succeed())
		patch := client.MergeFrom(r.DeepCopy())
		r.Status.Phase = databasev1.RestorePhaseScalingDown
		r.Status.OriginalReplicas = &replicas
		Expect(k8sClient.Status().Patch(ctx, r, patch)).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, sourceDepKey, dep)).To(Succeed())
		depStatusPatch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Replicas = 0
		Expect(k8sClient.Status().Patch(ctx, dep, depStatusPatch)).To(Succeed())
	}

	BeforeEach(func() {
		// Create the target Deployment if not present.
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, sourceDepKey, dep); err != nil && errors.IsNotFound(err) {
			replicas := int32(1)
			dep = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: sourceDepName, Namespace: namespaceName},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": sourceDepName}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": sourceDepName}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		}

		db := &databasev1.SQLiteDB{}
		if err := k8sClient.Get(ctx, sourceDBKey, db); err != nil && errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, newSourceDB())).To(Succeed())
		}

		// Create the Litestream ConfigMap (normally created by SQLiteDBReconciler).
		cm := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, sourceConfigMapKey, cm); err != nil && errors.IsNotFound(err) {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: sourceDBName + "-litestream", Namespace: namespaceName},
				Data:       map[string]string{"litestream.yml": "dbs: []\n"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())
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

		cm := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, sourceConfigMapKey, cm); err == nil {
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		}

		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, sourceDepKey, dep); err == nil {
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
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
		positionAtScalingDown(restoreKey)
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
		positionAtScalingDown(restoreKey)
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

		positionAtScalingDown(pitrKey)
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
		positionAtScalingDown(restoreKey)
		_, err := newRestoreReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseRunning))
		Expect(restore.Status.JobName).To(Equal(restoreName + "-restore"))
	})

	It("is idempotent — does not create a second Job on re-reconcile", func() {
		positionAtScalingDown(restoreKey)
		reconciler := newRestoreReconciler()

		// First reconcile: ScalingDown → Running (creates Job).
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Second reconcile in Running phase — Job already exists, should not error.
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

// ─────────────────────────────────────────────────────────────────────────────
// State machine tests — drive phase transitions with envtest.
// Each test creates its own isolated resources to avoid cross-test interference.
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("SQLiteRestore State Machine", func() {
	const (
		namespaceName = "default"
		targetPVC     = "sm-restore-pvc"
		targetPath    = "/data/myapp.db"
		secretRef     = "sm-s3-creds"
		bucketName    = "sm-backups"
		deployName    = "sm-app"
	)

	ctx := context.Background()

	// newStateMachineResources creates isolated resources for a state machine test:
	// a Deployment, a SQLiteDB, the Litestream ConfigMap (initially empty dbs list),
	// and a SQLiteRestore CR. Returns keys for all three for cleanup.
	newStateMachineResources := func(suffix string, replicas int32) (
		dbKey types.NamespacedName,
		restoreKey types.NamespacedName,
		deployKey types.NamespacedName,
	) {
		dbName := "sm-db-" + suffix
		restoreName := "sm-restore-" + suffix
		depName := deployName + "-" + suffix

		dbKey = types.NamespacedName{Name: dbName, Namespace: namespaceName}
		restoreKey = types.NamespacedName{Name: restoreName, Namespace: namespaceName}
		deployKey = types.NamespacedName{Name: depName, Namespace: namespaceName}

		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: namespaceName},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": depName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": depName}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		db := &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{Name: dbName, Namespace: namespaceName},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     "myapp.db",
				DatabasePath:     "/data",
				TargetDeployment: depName,
				Backup: databasev1.BackupSpec{
					Enabled: true,
					Destination: databasev1.BackupDestination{
						S3: &databasev1.S3Destination{
							Bucket:    bucketName,
							SecretRef: secretRef,
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, db)).To(Succeed())

		// Create the ConfigMap that reconcilePausing checks (normally created by SQLiteDBReconciler).
		// We create it with the full config; tests that simulate the pause will update it to dbs: [].
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: dbName + "-litestream", Namespace: namespaceName},
			Data:       map[string]string{"litestream.yml": "dbs:\n  - path: /data/myapp.db\n"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		restore := &databasev1.SQLiteRestore{
			ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespaceName},
			Spec: databasev1.SQLiteRestoreSpec{
				SourceRef:  dbName,
				TargetPVC:  targetPVC,
				TargetPath: targetPath,
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())

		return dbKey, restoreKey, deployKey
	}

	cleanupResources := func(dbKey, restoreKey, deployKey types.NamespacedName) {
		restore := &databasev1.SQLiteRestore{}
		if err := k8sClient.Get(ctx, restoreKey, restore); err == nil {
			_ = k8sClient.Delete(ctx, restore)
		}
		db := &databasev1.SQLiteDB{}
		if err := k8sClient.Get(ctx, dbKey, db); err == nil {
			_ = k8sClient.Delete(ctx, db)
		}
		cm := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: dbKey.Name + "-litestream", Namespace: namespaceName,
		}, cm); err == nil {
			_ = k8sClient.Delete(ctx, cm)
		}
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, deployKey, dep); err == nil {
			_ = k8sClient.Delete(ctx, dep)
		}
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: restoreKey.Name + "-restore", Namespace: namespaceName,
		}, job); err == nil {
			_ = k8sClient.Delete(ctx, job)
		}
	}

	newReconciler := func() *SQLiteRestoreReconciler {
		return &SQLiteRestoreReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(20),
		}
	}

	It("sets pause annotation on SQLiteDB and transitions to Pausing", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("pause-pending", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// SQLiteDB should now have the pause annotation.
		db := &databasev1.SQLiteDB{}
		Expect(k8sClient.Get(ctx, dbKey, db)).To(Succeed())
		Expect(db.Annotations[databasev1.AnnotationPause]).To(Equal("true"))

		// Restore should be in Pausing phase.
		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhasePausing))
	})

	It("records originalReplicas in status during Pending phase", func() {
		replicas := int32(3)
		dbKey, restoreKey, deployKey := newStateMachineResources("orig-replicas", replicas)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.OriginalReplicas).NotTo(BeNil())
		Expect(*restore.Status.OriginalReplicas).To(Equal(replicas))
	})

	It("scales Deployment to 0 when ConfigMap reflects pause (Pausing → ScalingDown)", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("scale-down", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()

		// Pending → Pausing.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Simulate controller reconciling the SQLiteDB and updating the ConfigMap.
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: dbKey.Name + "-litestream", Namespace: namespaceName,
		}, cm)).To(Succeed())
		cmPatch := client.MergeFrom(cm.DeepCopy())
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["litestream.yml"] = "dbs: []\n"
		Expect(k8sClient.Patch(ctx, cm, cmPatch)).To(Succeed())

		// Pausing → ScalingDown.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseScalingDown))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployKey, dep)).To(Succeed())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		Expect(*dep.Spec.Replicas).To(BeZero())
	})

	It("waits in ScalingDown while Deployment still has running replicas", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("wait-drain", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()

		// Drive to Pausing.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Update ConfigMap to reflect pause.
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: dbKey.Name + "-litestream", Namespace: namespaceName,
		}, cm)).To(Succeed())
		cmPatch := client.MergeFrom(cm.DeepCopy())
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["litestream.yml"] = "dbs: []\n"
		Expect(k8sClient.Patch(ctx, cm, cmPatch)).To(Succeed())

		// Drive to ScalingDown.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseScalingDown))

		// Simulate Deployment still draining (status.replicas = 1).
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployKey, dep)).To(Succeed())
		depStatusPatch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Replicas = 1
		Expect(k8sClient.Status().Patch(ctx, dep, depStatusPatch)).To(Succeed())

		// Reconcile should still be in ScalingDown.
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(restoreRequeueInterval))

		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseScalingDown))
		// Job should NOT exist yet.
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: restoreKey.Name + "-restore", Namespace: namespaceName,
		}, job)).To(MatchError(ContainSubstring("not found")))
	})

	It("creates Job and transitions to Running once Deployment reaches 0 replicas", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("create-job", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()

		// Drive to Pausing.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Update ConfigMap.
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: dbKey.Name + "-litestream", Namespace: namespaceName,
		}, cm)).To(Succeed())
		cmPatch := client.MergeFrom(cm.DeepCopy())
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["litestream.yml"] = "dbs: []\n"
		Expect(k8sClient.Patch(ctx, cm, cmPatch)).To(Succeed())

		// Drive to ScalingDown.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Simulate Deployment fully scaled down (status.replicas = 0).
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployKey, dep)).To(Succeed())
		depStatusPatch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Replicas = 0
		Expect(k8sClient.Status().Patch(ctx, dep, depStatusPatch)).To(Succeed())

		// ScalingDown → Running.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseRunning))

		// Job should exist.
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: restoreKey.Name + "-restore", Namespace: namespaceName,
		}, job)).To(Succeed())
	})

	It("transitions to ScalingUp when Job succeeds", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("job-complete", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()

		// Drive to Running.
		driveToRunning(ctx, reconciler, dbKey, restoreKey, deployKey)

		// Simulate Job success. Kubernetes 1.31+ requires SuccessCriteriaMet before Complete,
		// plus startTime and completionTime.
		jobKey := types.NamespacedName{Name: restoreKey.Name + "-restore", Namespace: namespaceName}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
		now := metav1.Now()
		jobStatusPatch := client.MergeFrom(job.DeepCopy())
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Patch(ctx, job, jobStatusPatch)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseScalingUp))
	})

	It("scales Deployment back to originalReplicas and transitions to Complete", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("scale-up", 2)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()
		driveToRunning(ctx, reconciler, dbKey, restoreKey, deployKey)

		// Simulate Job success with all required Kubernetes 1.31+ fields.
		jobKey := types.NamespacedName{Name: restoreKey.Name + "-restore", Namespace: namespaceName}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
		now := metav1.Now()
		jobStatusPatch := client.MergeFrom(job.DeepCopy())
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Patch(ctx, job, jobStatusPatch)).To(Succeed())

		// Running → ScalingUp.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// ScalingUp → Complete.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseComplete))

		// Deployment should be scaled back to originalReplicas.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployKey, dep)).To(Succeed())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
	})

	It("removes pause annotation after successful restore", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("remove-pause", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()
		driveToRunning(ctx, reconciler, dbKey, restoreKey, deployKey)

		jobKey := types.NamespacedName{Name: restoreKey.Name + "-restore", Namespace: namespaceName}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
		now := metav1.Now()
		jobStatusPatch := client.MergeFrom(job.DeepCopy())
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Patch(ctx, job, jobStatusPatch)).To(Succeed())

		// Running → ScalingUp → Complete.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// Pause annotation should be removed from SQLiteDB.
		db := &databasev1.SQLiteDB{}
		Expect(k8sClient.Get(ctx, dbKey, db)).To(Succeed())
		Expect(db.Annotations[databasev1.AnnotationPause]).NotTo(Equal("true"))
	})

	It("cleans up on Job failure: scales back up and removes pause annotation", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("fail-cleanup", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		reconciler := newReconciler()
		driveToRunning(ctx, reconciler, dbKey, restoreKey, deployKey)

		// Simulate Job failure. Kubernetes 1.31+ requires FailureTarget before Failed,
		// plus startTime.
		jobKey := types.NamespacedName{Name: restoreKey.Name + "-restore", Namespace: namespaceName}
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
		now := metav1.Now()
		jobStatusPatch := client.MergeFrom(job.DeepCopy())
		job.Status.StartTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Message: "simulated failure", LastProbeTime: now, LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "simulated failure", LastProbeTime: now, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Patch(ctx, job, jobStatusPatch)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseFailed))

		// Pause annotation should be removed.
		db := &databasev1.SQLiteDB{}
		Expect(k8sClient.Get(ctx, dbKey, db)).To(Succeed())
		Expect(db.Annotations[databasev1.AnnotationPause]).NotTo(Equal("true"))

		// Deployment should be scaled back up.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployKey, dep)).To(Succeed())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
	})

	It("is a no-op for Complete phase", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("noop-complete", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		// Force terminal phase directly.
		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		patch := client.MergeFrom(restore.DeepCopy())
		restore.Status.Phase = databasev1.RestorePhaseComplete
		Expect(k8sClient.Status().Patch(ctx, restore, patch)).To(Succeed())

		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		// SQLiteDB should NOT have pause annotation.
		db := &databasev1.SQLiteDB{}
		Expect(k8sClient.Get(ctx, dbKey, db)).To(Succeed())
		Expect(db.Annotations[databasev1.AnnotationPause]).NotTo(Equal("true"))
	})

	It("handles nil spec.replicas as originalReplicas=1", func() {
		dbKey, restoreKey, deployKey := newStateMachineResources("nil-replicas", 1)
		defer cleanupResources(dbKey, restoreKey, deployKey)

		// Clear spec.replicas (nil → Kubernetes defaults to 1).
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployKey, dep)).To(Succeed())
		depPatch := client.MergeFrom(dep.DeepCopy())
		dep.Spec.Replicas = nil
		Expect(k8sClient.Patch(ctx, dep, depPatch)).To(Succeed())

		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
		Expect(err).NotTo(HaveOccurred())

		restore := &databasev1.SQLiteRestore{}
		Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
		Expect(restore.Status.OriginalReplicas).NotTo(BeNil())
		Expect(*restore.Status.OriginalReplicas).To(Equal(int32(1)))
	})

	It("proceeds gracefully when SQLiteDB's targetDeployment does not exist", func() {
		// A missing Deployment is a valid case: the app may have been torn down
		// before the restore was requested. The controller skips scale-down and
		// proceeds to create the restore Job with originalReplicas=0.
		dbName := "sm-db-missing-dep"
		restoreName := "sm-restore-missing-dep"

		db := &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{Name: dbName, Namespace: namespaceName},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     "myapp.db",
				DatabasePath:     "/data",
				TargetDeployment: "nonexistent-deployment",
				Backup: databasev1.BackupSpec{
					Enabled: true,
					Destination: databasev1.BackupDestination{
						S3: &databasev1.S3Destination{Bucket: "b", SecretRef: "s"},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, db) }()

		// Create the ConfigMap that Pausing phase checks.
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: dbName + "-litestream", Namespace: namespaceName},
			Data:       map[string]string{"litestream.yml": "dbs: []\n"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cm) }()

		restore := &databasev1.SQLiteRestore{
			ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespaceName},
			Spec: databasev1.SQLiteRestoreSpec{
				SourceRef:  dbName,
				TargetPVC:  targetPVC,
				TargetPath: targetPath,
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, restore)
			job := &batchv1.Job{}
			_ = k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: restoreName + "-restore", Namespace: namespaceName,
			}})
			_ = job.Name // suppress unused variable
		}()

		reconciler := newReconciler()

		// Pending → Pausing (deployment not found → originalReplicas=0)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: restoreName, Namespace: namespaceName},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restoreName, Namespace: namespaceName}, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhasePausing))
		Expect(restore.Status.OriginalReplicas).NotTo(BeNil())
		Expect(*restore.Status.OriginalReplicas).To(Equal(int32(0)))

		// Pausing → ScalingDown (deployment not found → skip scale, go straight to ScalingDown)
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: restoreName, Namespace: namespaceName},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restoreName, Namespace: namespaceName}, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseScalingDown))

		// ScalingDown → Running (deployment not found → treat as replicas=0, create Job)
		_, err = reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: restoreName, Namespace: namespaceName},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restoreName, Namespace: namespaceName}, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseRunning))

		// Job should exist.
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: restoreName + "-restore", Namespace: namespaceName,
		}, job)).To(Succeed())
		_ = k8sClient.Delete(ctx, job)
	})
})

// driveToRunning drives a restore through Pending → Pausing → ScalingDown → Running
// by simulating ConfigMap update and Deployment scale-down completion.
func driveToRunning(
	ctx context.Context,
	reconciler *SQLiteRestoreReconciler,
	dbKey, restoreKey, deployKey types.NamespacedName,
) {
	// Pending → Pausing.
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
	Expect(err).NotTo(HaveOccurred())

	// Simulate ConfigMap updated to dbs: [].
	cm := &corev1.ConfigMap{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: dbKey.Name + "-litestream", Namespace: dbKey.Namespace,
	}, cm)).To(Succeed())
	cmPatch := client.MergeFrom(cm.DeepCopy())
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["litestream.yml"] = "dbs: []\n"
	Expect(k8sClient.Patch(ctx, cm, cmPatch)).To(Succeed())

	// Pausing → ScalingDown.
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
	Expect(err).NotTo(HaveOccurred())

	// Simulate Deployment fully scaled down.
	dep := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, deployKey, dep)).To(Succeed())
	depStatusPatch := client.MergeFrom(dep.DeepCopy())
	dep.Status.Replicas = 0
	Expect(k8sClient.Status().Patch(ctx, dep, depStatusPatch)).To(Succeed())

	// ScalingDown → Running.
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: restoreKey})
	Expect(err).NotTo(HaveOccurred())

	restore := &databasev1.SQLiteRestore{}
	Expect(k8sClient.Get(ctx, restoreKey, restore)).To(Succeed())
	Expect(restore.Status.Phase).To(Equal(databasev1.RestorePhaseRunning))
}
