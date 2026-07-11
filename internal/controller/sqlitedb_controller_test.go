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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

var _ = Describe("SQLiteDB Controller", func() {
	const (
		resourceName   = "test-sqlitedb"
		deploymentName = "test-app"
		namespaceName  = "default"
		databaseName   = "myapp.db"
		databasePath   = "/data"
		appLabel       = "app"
	)

	ctx := context.Background()
	namespacedName := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
	deploymentKey := types.NamespacedName{Name: deploymentName, Namespace: namespaceName}
	cmKey := types.NamespacedName{Name: resourceName + "-litestream", Namespace: namespaceName}

	BeforeEach(func() {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespaceName},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{appLabel: deploymentName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabel: deploymentName}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		db := &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespaceName},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     databaseName,
				DatabasePath:     databasePath,
				TargetDeployment: deploymentName,
				Backup:           databasev1.BackupSpec{Enabled: false},
			},
		}
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
	})

	AfterEach(func() {
		// Delete ConfigMaps explicitly — envtest does not GC owned objects.
		for _, name := range []string{resourceName + "-litestream", resourceName + "-init-sql"} {
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespaceName}, cm); err == nil {
				_ = k8sClient.Delete(ctx, cm)
			}
		}
		db := &databasev1.SQLiteDB{}
		if err := k8sClient.Get(ctx, namespacedName, db); err == nil {
			Expect(k8sClient.Delete(ctx, db)).To(Succeed())
		}
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, deploymentKey, dep); err == nil {
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		}
	})

	It("should create the Litestream ConfigMap", func() {
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKey("litestream.yml"))
			g.Expect(cm.Data["litestream.yml"]).To(ContainSubstring(databasePath + "/" + databaseName))
		}).Should(Succeed())
	})

	It("should annotate the target Deployment's pod template", func() {
		Eventually(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, deploymentKey, dep)).To(Succeed())
			g.Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(injectAnnotation, "true"))
			g.Expect(dep.Spec.Template.Annotations).To(HaveKey(configAnnotation))
			g.Expect(dep.Spec.Template.Labels).To(HaveKeyWithValue(injectAnnotation, "true"))
		}).Should(Succeed())
	})

	It("should set SidecarInjected condition after annotation", func() {
		Eventually(func(g Gomega) {
			db := &databasev1.SQLiteDB{}
			g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			cond := meta.FindStatusCondition(db.Status.Conditions, databasev1.ConditionSidecarInjected)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}).Should(Succeed())
	})

	It("should set BackupHealthy condition to False when backup is disabled", func() {
		Eventually(func(g Gomega) {
			db := &databasev1.SQLiteDB{}
			g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			cond := meta.FindStatusCondition(db.Status.Conditions, databasev1.ConditionBackupHealthy)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("BackupDisabled"))
		}).Should(Succeed())
	})

	It("should set BackupHealthy to False when no Litestream pods exist yet", func() {
		// Wait for initial reconcile, then enable backup.
		Eventually(func(g Gomega) {
			db := &databasev1.SQLiteDB{}
			g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			g.Expect(db.Status.Conditions).NotTo(BeEmpty())
		}).Should(Succeed())

		db := &databasev1.SQLiteDB{}
		Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
		db.Spec.Backup = databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{Bucket: "test", SecretRef: "creds"},
			},
		}
		Expect(k8sClient.Update(ctx, db)).To(Succeed())

		Eventually(func(g Gomega) {
			db := &databasev1.SQLiteDB{}
			g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			cond := meta.FindStatusCondition(db.Status.Conditions, databasev1.ConditionBackupHealthy)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("SidecarUnhealthy"))
		}).Should(Succeed())
	})

	It("should requeue after the status sync interval", func() {
		// Call Reconcile directly to inspect the return value — the manager itself
		// does not expose its RequeueAfter, so this tests the contract directly.
		r := &SQLiteDBReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1),
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(statusSyncInterval))
	})

	Context("replication pause", func() {
		It("produces empty dbs config when pause annotation is set", func() {
			// Wait for the manager to create the initial ConfigMap.
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			}).Should(Succeed())

			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			if db.Annotations == nil {
				db.Annotations = map[string]string{}
			}
			db.Annotations[databasev1.AnnotationPause] = "true"
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
				g.Expect(cm.Data["litestream.yml"]).To(Equal("dbs: []\n"))
			}).Should(Succeed())
		})

		It("produces normal config when pause annotation is absent", func() {
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
				g.Expect(cm.Data["litestream.yml"]).To(ContainSubstring(databasePath + "/" + databaseName))
				g.Expect(cm.Data["litestream.yml"]).NotTo(Equal("dbs: []\n"))
			}).Should(Succeed())
		})

		It("reverts config to normal after pause annotation is removed", func() {
			// Wait for initial ConfigMap.
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			}).Should(Succeed())

			// Set pause.
			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			if db.Annotations == nil {
				db.Annotations = map[string]string{}
			}
			db.Annotations[databasev1.AnnotationPause] = "true"
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
				g.Expect(cm.Data["litestream.yml"]).To(Equal("dbs: []\n"))
			}).Should(Succeed())

			// Remove pause.
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			delete(db.Annotations, databasev1.AnnotationPause)
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
				g.Expect(cm.Data["litestream.yml"]).To(ContainSubstring(databasePath + "/" + databaseName))
			}).Should(Succeed())
		})

		It("sets ReplicationPaused condition to True when pause annotation is set", func() {
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			}).Should(Succeed())

			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			if db.Annotations == nil {
				db.Annotations = map[string]string{}
			}
			db.Annotations[databasev1.AnnotationPause] = "true"
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				cond := meta.FindStatusCondition(db.Status.Conditions, databasev1.ConditionReplicationPaused)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.Reason).To(Equal("PauseAnnotationSet"))
			}).Should(Succeed())
		})

		It("sets ReplicationPaused condition to False when pause annotation is absent", func() {
			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				cond := meta.FindStatusCondition(db.Status.Conditions, databasev1.ConditionReplicationPaused)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("ReplicationActive"))
			}).Should(Succeed())
		})

		It("sets phase to Paused when pause annotation is set", func() {
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			}).Should(Succeed())

			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			if db.Annotations == nil {
				db.Annotations = map[string]string{}
			}
			db.Annotations[databasev1.AnnotationPause] = "true"
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				g.Expect(db.Status.Phase).To(Equal(databasev1.PhasePaused))
			}).Should(Succeed())
		})

		It("reverts phase from Paused when pause annotation is removed", func() {
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			}).Should(Succeed())

			// Set pause.
			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			if db.Annotations == nil {
				db.Annotations = map[string]string{}
			}
			db.Annotations[databasev1.AnnotationPause] = "true"
			Expect(k8sClient.Update(ctx, db)).To(Succeed())
			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				g.Expect(db.Status.Phase).To(Equal(databasev1.PhasePaused))
			}).Should(Succeed())

			// Remove pause.
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			delete(db.Annotations, databasev1.AnnotationPause)
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				g.Expect(db.Status.Phase).NotTo(Equal(databasev1.PhasePaused))
			}).Should(Succeed())
		})
	})

	Context("init SQL management", func() {
		const initSQL = "CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY);"

		It("creates the init-sql ConfigMap when InitSQL is set", func() {
			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			db.Spec.InitSQL = initSQL
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: resourceName + "-init-sql", Namespace: namespaceName,
				}, cm)).To(Succeed())
				g.Expect(cm.Data["init.sql"]).To(Equal(initSQL))
			}).Should(Succeed())
		})

		It("records the SHA-256 hash in status.InitSQLHash", func() {
			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			db.Spec.InitSQL = initSQL
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				g.Expect(db.Status.InitSQLHash).To(Equal(initSQLHash(initSQL)))
			}).Should(Succeed())
		})

		It("updates the ConfigMap and hash when InitSQL changes", func() {
			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			db.Spec.InitSQL = initSQL
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				g.Expect(db.Status.InitSQLHash).To(Equal(initSQLHash(initSQL)))
			}).Should(Succeed())

			// Change the SQL.
			newSQL := initSQL + "\nCREATE TABLE IF NOT EXISTS posts (id INTEGER PRIMARY KEY);"
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			db.Spec.InitSQL = newSQL
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: resourceName + "-init-sql", Namespace: namespaceName,
				}, cm)).To(Succeed())
				g.Expect(cm.Data["init.sql"]).To(Equal(newSQL))

				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				g.Expect(db.Status.InitSQLHash).To(Equal(initSQLHash(newSQL)))
			}).Should(Succeed())
		})

		It("sets InitSQLApplied condition to True when ConfigMap is ready", func() {
			db := &databasev1.SQLiteDB{}
			Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
			db.Spec.InitSQL = initSQL
			Expect(k8sClient.Update(ctx, db)).To(Succeed())

			Eventually(func(g Gomega) {
				db := &databasev1.SQLiteDB{}
				g.Expect(k8sClient.Get(ctx, namespacedName, db)).To(Succeed())
				cond := meta.FindStatusCondition(db.Status.Conditions, databasev1.ConditionInitSQLApplied)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.Reason).To(Equal("ConfigMapReady"))
			}).Should(Succeed())
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// Pure-function and helper tests — no envtest required for most.
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("isFailureReason", func() {
	DescribeTable("classifies container waiting reasons correctly",
		func(reason string, expected bool) {
			Expect(isFailureReason(reason)).To(Equal(expected))
		},
		Entry("CrashLoopBackOff → true", "CrashLoopBackOff", true),
		Entry("OOMKilled → true", "OOMKilled", true),
		Entry("Error → true", "Error", true),
		Entry("ImagePullBackOff → true", "ImagePullBackOff", true),
		Entry("ErrImagePull → true", "ErrImagePull", true),
		Entry("ContainerCreating → false", "ContainerCreating", false),
		Entry("PodInitializing → false", "PodInitializing", false),
		Entry("Running → false", "Running", false),
		Entry("empty string → false", "", false),
	)
})

var _ = Describe("buildLitestreamConfig", func() {
	var reconciler *SQLiteDBReconciler

	BeforeEach(func() {
		reconciler = &SQLiteDBReconciler{}
	})

	newDB := func(path, name string, backup databasev1.BackupSpec) *databasev1.SQLiteDB {
		return &databasev1.SQLiteDB{
			Spec: databasev1.SQLiteDBSpec{
				DatabasePath: path,
				DatabaseName: name,
				Backup:       backup,
			},
		}
	}

	It("produces minimal config when backup is disabled", func() {
		db := newDB("/data", "app.db", databasev1.BackupSpec{Enabled: false})
		cfg := reconciler.buildLitestreamConfig(db)
		Expect(cfg).To(ContainSubstring("dbs:"))
		Expect(cfg).To(ContainSubstring("/data/app.db"))
		Expect(cfg).NotTo(ContainSubstring("replica:"))
		Expect(cfg).NotTo(ContainSubstring("type: s3"))
	})

	It("includes full S3 replica block when backup is enabled", func() {
		db := newDB("/data", "app.db", databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{Bucket: "mybucket", SecretRef: "creds"},
			},
		})
		cfg := reconciler.buildLitestreamConfig(db)
		Expect(cfg).To(ContainSubstring("type: s3"))
		Expect(cfg).To(ContainSubstring("bucket: mybucket"))
		Expect(cfg).NotTo(ContainSubstring("endpoint:"))        // no endpoint for AWS S3
		Expect(cfg).NotTo(ContainSubstring("force-path-style")) // no force-path-style without endpoint
	})

	It("auto-prefixes endpoint with http:// when no scheme present", func() {
		db := newDB("/data", "app.db", databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{
					Endpoint:  "minio.homelab:9000",
					Bucket:    "b",
					SecretRef: "s",
				},
			},
		})
		cfg := reconciler.buildLitestreamConfig(db)
		Expect(cfg).To(ContainSubstring("endpoint: http://minio.homelab:9000"))
		Expect(cfg).To(ContainSubstring("force-path-style: true"))
	})

	It("preserves https:// scheme and does not double-prefix", func() {
		db := newDB("/data", "app.db", databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{
					Endpoint:  "https://s3.example.com",
					Bucket:    "b",
					SecretRef: "s",
				},
			},
		})
		cfg := reconciler.buildLitestreamConfig(db)
		Expect(cfg).To(ContainSubstring("endpoint: https://s3.example.com"))
		Expect(cfg).NotTo(ContainSubstring("http://https://"))
	})

	It("preserves http:// scheme and does not double-prefix", func() {
		db := newDB("/data", "app.db", databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{
					Endpoint:  "http://minio:9000",
					Bucket:    "b",
					SecretRef: "s",
				},
			},
		})
		cfg := reconciler.buildLitestreamConfig(db)
		Expect(cfg).To(ContainSubstring("endpoint: http://minio:9000"))
		Expect(cfg).NotTo(ContainSubstring("http://http://"))
	})

	It("includes path when set", func() {
		db := newDB("/data", "app.db", databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{
					Bucket:    "b",
					Path:      "myapp/",
					SecretRef: "s",
				},
			},
		})
		cfg := reconciler.buildLitestreamConfig(db)
		Expect(cfg).To(ContainSubstring("path: myapp/"))
	})

	It("includes retention when set", func() {
		db := newDB("/data", "app.db", databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{Bucket: "b", SecretRef: "s"},
			},
			Retention: databasev1.RetentionPolicy{Duration: "168h"},
		})
		cfg := reconciler.buildLitestreamConfig(db)
		Expect(cfg).To(ContainSubstring("retention: 168h"))
	})
})

var _ = Describe("litestreamContainerState and archiveCheckState", func() {
	// These tests use envtest to create Pods with manually-patched container statuses.
	const (
		lsTestNamespace = "default"
		lsDepName       = "ls-state-app"
		lsDBName        = "ls-state-db"
		lsSecretRef     = "ls-creds"
	)

	ctx := context.Background()

	var (
		deployment *appsv1.Deployment
		sqliteDB   *databasev1.SQLiteDB
		reconciler *SQLiteDBReconciler
	)

	BeforeEach(func() {
		reconciler = &SQLiteDBReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(10),
		}

		replicas := int32(1)
		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: lsDepName, Namespace: lsTestNamespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": lsDepName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": lsDepName}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		sqliteDB = &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{Name: lsDBName, Namespace: lsTestNamespace},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     "app.db",
				DatabasePath:     "/data",
				TargetDeployment: lsDepName,
				Backup: databasev1.BackupSpec{
					Enabled: true,
					Destination: databasev1.BackupDestination{
						S3: &databasev1.S3Destination{Bucket: "b", SecretRef: lsSecretRef},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sqliteDB)).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, sqliteDB)
		_ = k8sClient.Delete(ctx, deployment)
		// Clean up any pods created during tests.
		podList := &corev1.PodList{}
		_ = k8sClient.List(ctx, podList,
			client.InNamespace(lsTestNamespace),
			client.MatchingLabels{"app": lsDepName})
		for i := range podList.Items {
			_ = k8sClient.Delete(ctx, &podList.Items[i])
		}
	})

	Describe("litestreamContainerState", func() {
		It("returns healthy=false when no pods exist", func() {
			healthy, msg := reconciler.litestreamContainerState(ctx, sqliteDB, deployment)
			Expect(healthy).To(BeFalse())
			Expect(msg).To(ContainSubstring("no pods with Litestream sidecar found"))
		})

		It("returns healthy=true when sidecar is Running", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ls-running-pod",
					Namespace: lsTestNamespace,
					Labels:    map[string]string{"app": lsDepName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			now := metav1.Now()
			patch := client.MergeFrom(pod.DeepCopy())
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{
					Name:  "litestream",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}},
				},
			}
			Expect(k8sClient.Status().Patch(ctx, pod, patch)).To(Succeed())

			healthy, msg := reconciler.litestreamContainerState(ctx, sqliteDB, deployment)
			Expect(healthy).To(BeTrue())
			Expect(msg).To(ContainSubstring("running in 1 pod"))
		})

		It("returns healthy=false when sidecar is in CrashLoopBackOff", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ls-crashloop-pod",
					Namespace: lsTestNamespace,
					Labels:    map[string]string{"app": lsDepName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			patch := client.MergeFrom(pod.DeepCopy())
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{
					Name: "litestream",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
				},
			}
			Expect(k8sClient.Status().Patch(ctx, pod, patch)).To(Succeed())

			healthy, msg := reconciler.litestreamContainerState(ctx, sqliteDB, deployment)
			Expect(healthy).To(BeFalse())
			Expect(msg).To(ContainSubstring("unhealthy"))
		})

		It("returns healthy=false when sidecar terminated with non-zero exit", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ls-terminated-pod",
					Namespace: lsTestNamespace,
					Labels:    map[string]string{"app": lsDepName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			patch := client.MergeFrom(pod.DeepCopy())
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{
					Name: "litestream",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
					},
				},
			}
			Expect(k8sClient.Status().Patch(ctx, pod, patch)).To(Succeed())

			healthy, msg := reconciler.litestreamContainerState(ctx, sqliteDB, deployment)
			Expect(healthy).To(BeFalse())
			Expect(msg).To(ContainSubstring("unhealthy"))
		})
	})

	Describe("archiveCheckState", func() {
		It("returns (false, passed) when backup is disabled", func() {
			sqliteDB.Spec.Backup.Enabled = false
			failed, msg := reconciler.archiveCheckState(ctx, sqliteDB, deployment)
			Expect(failed).To(BeFalse())
			Expect(msg).To(Equal("backup not enabled"))
		})

		It("returns (false, passed) when no pods exist", func() {
			failed, msg := reconciler.archiveCheckState(ctx, sqliteDB, deployment)
			Expect(failed).To(BeFalse())
			Expect(msg).To(Equal("archive check passed"))
		})

		It("returns (true, failed) when archive-check init container exited non-zero", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "archive-check-fail-pod",
					Namespace: lsTestNamespace,
					Labels:    map[string]string{"app": lsDepName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			patch := client.MergeFrom(pod.DeepCopy())
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
				{
					Name: "litestream-archive-check",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
					},
				},
			}
			Expect(k8sClient.Status().Patch(ctx, pod, patch)).To(Succeed())

			failed, msg := reconciler.archiveCheckState(ctx, sqliteDB, deployment)
			Expect(failed).To(BeTrue())
			Expect(msg).To(ContainSubstring("archive check failed"))
		})

		It("returns (false, passed) when archive-check init container exited zero", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "archive-check-pass-pod",
					Namespace: lsTestNamespace,
					Labels:    map[string]string{"app": lsDepName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			patch := client.MergeFrom(pod.DeepCopy())
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
				{
					Name: "litestream-archive-check",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
					},
				},
			}
			Expect(k8sClient.Status().Patch(ctx, pod, patch)).To(Succeed())

			failed, msg := reconciler.archiveCheckState(ctx, sqliteDB, deployment)
			Expect(failed).To(BeFalse())
			Expect(msg).To(Equal("archive check passed"))
		})
	})
})
