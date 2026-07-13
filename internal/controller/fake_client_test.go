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

// Package controller contains fake-client error-injection tests.
// These tests use controller-runtime's fake client with WithInterceptorFuncs to
// exercise error paths (non-NotFound Get failures, Patch failures, etc.) that
// the live envtest environment cannot inject without API server cooperation.
package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

// errTransient is a sentinel non-NotFound error used to simulate transient API failures.
var errTransient = fmt.Errorf("transient API error") //nolint:staticcheck

// notFoundErr returns a genuine NotFound status error for the given resource.
func notFoundErr(resource, name string) error {
	return errors.NewNotFound(schema.GroupResource{Resource: resource}, name)
}

// fakeDB returns a minimal SQLiteDB with backup enabled for fake client tests.
func fakeDB(name, namespace, targetDep string) *databasev1.SQLiteDB {
	return &databasev1.SQLiteDB{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: databasev1.SQLiteDBSpec{
			DatabaseName:     "app.db",
			DatabasePath:     "/data",
			TargetDeployment: targetDep,
			Backup: databasev1.BackupSpec{
				Enabled: true,
				Destination: databasev1.BackupDestination{
					S3: &databasev1.S3Destination{Bucket: "b", SecretRef: "s"},
				},
			},
		},
	}
}

// fakeRestore returns a minimal SQLiteRestore for fake client tests.
func fakeRestore(name, namespace, sourceRef string, phase string) *databasev1.SQLiteRestore {
	r := &databasev1.SQLiteRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: databasev1.SQLiteRestoreSpec{
			SourceRef:  sourceRef,
			TargetPVC:  "my-pvc",
			TargetPath: "/data/app.db",
		},
	}
	r.Status.Phase = phase
	return r
}

// fakeDeployment returns a minimal single-replica Deployment.
func fakeDeployment(name, namespace string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
			},
		},
	}
}

// newFakeDBReconciler builds a SQLiteDBReconciler backed by a fake client.
func newFakeDBReconciler(objs []client.Object, funcs interceptor.Funcs) *SQLiteDBReconciler {
	fc := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(statusSubresourceObjs(objs)...).
		WithInterceptorFuncs(funcs).
		Build()
	return &SQLiteDBReconciler{
		Client:   fc,
		Scheme:   scheme.Scheme,
		Recorder: record.NewFakeRecorder(10),
	}
}

// newFakeRestoreReconciler builds a SQLiteRestoreReconciler backed by a fake client.
func newFakeRestoreReconciler(objs []client.Object, funcs interceptor.Funcs) *SQLiteRestoreReconciler {
	fc := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(statusSubresourceObjs(objs)...).
		WithInterceptorFuncs(funcs).
		Build()
	return &SQLiteRestoreReconciler{
		Client:   fc,
		Scheme:   scheme.Scheme,
		Recorder: record.NewFakeRecorder(10),
	}
}

// statusSubresourceObjs extracts objects that need status subresource registration.
func statusSubresourceObjs(objs []client.Object) []client.Object {
	var out []client.Object
	for _, o := range objs {
		switch o.(type) {
		case *databasev1.SQLiteDB, *databasev1.SQLiteRestore:
			out = append(out, o)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// SQLiteDB error-injection tests
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("SQLiteDBReconciler error injection", func() {
	const (
		ns      = "default"
		dbName  = "fake-db"
		depName = "fake-dep"
	)
	ctx := context.Background()
	key := reconcile.Request{NamespacedName: types.NamespacedName{Name: dbName, Namespace: ns}}

	It("Reconcile propagates non-NotFound error from Get(SQLiteDB)", func() {
		r := newFakeDBReconciler(nil, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*databasev1.SQLiteDB); ok {
					return errTransient
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, key)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("reconcileLitestreamConfig propagates ConfigMap Create error", func() {
		db := fakeDB(dbName, ns, depName)
		// The ConfigMap does not pre-exist, so controllerutil.CreateOrUpdate calls Create.
		r := newFakeDBReconciler([]client.Object{db}, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if _, ok := o.(*corev1.ConfigMap); ok {
					return errTransient
				}
				return c.Create(ctx, o, opts...)
			},
		})
		err := r.reconcileLitestreamConfig(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("reconcileInitSQLConfig propagates Status().Patch error when clearing stale hash", func() {
		db := fakeDB(dbName, ns, depName)
		db.Status.InitSQLHash = "stale-hash"
		// InitSQL is empty so the "clear stale hash" path is taken.
		db.Spec.InitSQL = ""
		r := newFakeDBReconciler([]client.Object{db}, interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				return errTransient
			},
		})
		err := r.reconcileInitSQLConfig(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("updateStatus propagates Status().Patch error on WorkloadNotFound path", func() {
		// DB targets a Deployment that doesn't exist in the fake client.
		db := fakeDB(dbName, ns, "nonexistent-dep")
		r := newFakeDBReconciler([]client.Object{db}, interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				return errTransient
			},
		})
		err := r.updateStatus(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("updateStatus propagates Status().Patch error on ReplicaCountExceeded path", func() {
		replicas := int32(3)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": depName}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": depName}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
				},
			},
		}
		db := fakeDB(dbName, ns, depName)
		r := newFakeDBReconciler([]client.Object{dep, db}, interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				return errTransient
			},
		})
		err := r.updateStatus(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("reconcileTargetAnnotation propagates Patch error when annotating workload", func() {
		dep := fakeDeployment(depName, ns)
		db := fakeDB(dbName, ns, depName)
		r := newFakeDBReconciler([]client.Object{dep, db}, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := o.(*appsv1.Deployment); ok {
					return errTransient
				}
				return c.Patch(ctx, o, p, opts...)
			},
		})
		err := r.reconcileTargetAnnotation(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// SQLiteRestoreReconciler error-injection tests
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("SQLiteRestoreReconciler error injection", func() {
	const (
		ns          = "default"
		dbName      = "fake-src-db"
		depName     = "fake-src-dep"
		restoreName = "fake-restore"
	)
	ctx := context.Background()
	restoreKey := reconcile.Request{NamespacedName: types.NamespacedName{Name: restoreName, Namespace: ns}}

	// litestreamCM returns a ConfigMap that represents the Litestream config for dbName.
	litestreamCM := func() *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: dbName + "-litestream", Namespace: ns},
			Data:       map[string]string{"litestream.yml": "addr: \":9090\"\ndbs:\n  - path: /data/app.db\n    replica:\n      type: s3\n      bucket: b\n"},
		}
	}

	It("Reconcile propagates non-NotFound error from Get(SQLiteRestore)", func() {
		r := newFakeRestoreReconciler(nil, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*databasev1.SQLiteRestore); ok {
					return errTransient
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("Reconcile propagates failRestore when Get(SQLiteDB) returns NotFound", func() {
		restore := fakeRestore(restoreName, ns, dbName, "")
		r := newFakeRestoreReconciler([]client.Object{restore}, interceptor.Funcs{})
		// SQLiteDB does not exist — Get returns NotFound → failRestore → sets Failed status.
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).NotTo(HaveOccurred()) // failRestore swallows the error and patches status
		got := &databasev1.SQLiteRestore{}
		Expect(r.Client.Get(ctx, types.NamespacedName{Name: restoreName, Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(databasev1.RestorePhaseFailed))
		Expect(got.Status.Message).To(ContainSubstring("not found"))
	})

	It("reconcilePending propagates error when pauseReplication Patch fails", func() {
		db := fakeDB(dbName, ns, depName)
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, "")
		cm := litestreamCM()

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore, cm}, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				// Fail the Patch on the SQLiteDB (which is what pauseReplication does).
				if _, ok := o.(*databasev1.SQLiteDB); ok {
					return errTransient
				}
				return c.Patch(ctx, o, p, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("setting pause annotation"))
	})

	It("reconcilePausing propagates error when Get(ConfigMap) fails", func() {
		db := fakeDB(dbName, ns, depName)
		// Set pause annotation so reconcilePausing skips the re-set-annotation branch.
		db.Annotations = map[string]string{pauseAnnotation: injectEnabled}
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhasePausing)
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore}, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*corev1.ConfigMap); ok {
					return errTransient
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("getting litestream ConfigMap"))
	})

	It("reconcilePausing propagates error when scaleWorkload Patch fails", func() {
		db := fakeDB(dbName, ns, depName)
		db.Annotations = map[string]string{pauseAnnotation: injectEnabled}
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhasePausing)
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas
		// ConfigMap with paused content so the CM check passes.
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: dbName + "-litestream", Namespace: ns},
			Data:       map[string]string{"litestream.yml": pausedConfig},
		}

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore, cm}, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := o.(*appsv1.Deployment); ok {
					return errTransient
				}
				return c.Patch(ctx, o, p, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("scaling workload to 0"))
	})

	It("reconcileScalingDown propagates error when Create(restore Job) fails", func() {
		db := fakeDB(dbName, ns, depName)
		db.Annotations = map[string]string{pauseAnnotation: injectEnabled}
		dep := fakeDeployment(depName, ns)
		replicas := int32(1)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhaseScalingDown)
		restore.Status.OriginalReplicas = &replicas
		restore.Status.JobName = restoreName + "-restore"
		cm := litestreamCM()

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore, cm}, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if _, ok := o.(*batchv1.Job); ok {
					return errTransient
				}
				return c.Create(ctx, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("creating restore Job"))
	})

	It("reconcileRunning propagates error when Get(restore Job) fails with non-NotFound", func() {
		db := fakeDB(dbName, ns, depName)
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhaseRunning)
		restore.Status.JobName = restoreName + "-restore"
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore}, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*batchv1.Job); ok {
					return errTransient
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("getting restore Job"))
	})

	It("reconcileRunning calls failRestoreWithCleanup when restore Job is NotFound", func() {
		db := fakeDB(dbName, ns, depName)
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhaseRunning)
		restore.Status.JobName = restoreName + "-restore"
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas

		// Job is NOT in the fake client — Get returns NotFound.
		r := newFakeRestoreReconciler([]client.Object{db, dep, restore}, interceptor.Funcs{})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).NotTo(HaveOccurred()) // failRestoreWithCleanup patches status, doesn't return error

		got := &databasev1.SQLiteRestore{}
		Expect(r.Client.Get(ctx, types.NamespacedName{Name: restoreName, Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(databasev1.RestorePhaseFailed))
		Expect(got.Status.Message).To(ContainSubstring("not found"))
	})

	It("reconcileValidating propagates error when Create(validation Job) fails", func() {
		db := fakeDB(dbName, ns, depName)
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhaseValidating)
		restore.Status.JobName = restoreName + "-restore"
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore}, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if _, ok := o.(*batchv1.Job); ok {
					return errTransient
				}
				return c.Create(ctx, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("creating validation Job"))
	})

	It("reconcileValidating propagates error when Get(validation Job) fails with non-NotFound", func() {
		db := fakeDB(dbName, ns, depName)
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhaseValidating)
		restore.Status.JobName = restoreName + "-restore"
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas

		// Validation Job already exists (so Create is not called).
		validationJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      restoreName + "-validate",
				Namespace: ns,
				Labels:    map[string]string{restoreLabelKey: restoreName},
			},
		}

		callCount := 0
		r := newFakeRestoreReconciler([]client.Object{db, dep, restore, validationJob}, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*batchv1.Job); ok {
					callCount++
					// First call is the validation job lookup in reconcileValidating.
					// Return a non-NotFound error to exercise the error path.
					return errTransient
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("getting validation Job"))
		Expect(callCount).To(BeNumerically(">=", 1))
	})

	It("reconcileScalingUp propagates error when resumeReplication Get(SQLiteDB) fails", func() {
		db := fakeDB(dbName, ns, depName)
		db.Annotations = map[string]string{pauseAnnotation: injectEnabled}
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhaseScalingUp)
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas

		// Allow Get for everything except SQLiteDB (to make resumeReplication fail).
		// The initial Reconcile() Get on SQLiteRestore and SQLiteDB must succeed,
		// but the re-fetch inside resumeReplication should fail.
		dbGetCount := 0
		r := newFakeRestoreReconciler([]client.Object{db, dep, restore}, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*databasev1.SQLiteDB); ok {
					dbGetCount++
					// First call (initial Reconcile lookup) must succeed.
					// Second call is the re-fetch inside resumeReplication — fail it.
					if dbGetCount > 1 {
						return errTransient
					}
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("removing pause annotation"))
	})

	It("pauseReplication propagates Patch error", func() {
		db := fakeDB(dbName, ns, depName)
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhasePausing)
		// Pause annotation NOT set → pauseReplication will Patch.
		replicas := int32(1)
		restore.Status.OriginalReplicas = &replicas
		cm := litestreamCM()

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore, cm}, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := o.(*databasev1.SQLiteDB); ok {
					return errTransient
				}
				return c.Patch(ctx, o, p, opts...)
			},
		})
		// reconcilePausing calls pauseReplication when the annotation is absent → Patch fails.
		err := r.pauseReplication(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("resumeReplication propagates Get error when re-fetching SQLiteDB", func() {
		db := fakeDB(dbName, ns, depName)
		db.Annotations = map[string]string{pauseAnnotation: injectEnabled}

		r := newFakeRestoreReconciler([]client.Object{db}, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*databasev1.SQLiteDB); ok {
					return errTransient
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		err := r.resumeReplication(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("resumeReplication propagates Patch error when removing pause annotation", func() {
		db := fakeDB(dbName, ns, depName)
		db.Annotations = map[string]string{pauseAnnotation: injectEnabled}

		// Allow Get but fail the Patch.
		dbGetCount := 0
		r := newFakeRestoreReconciler([]client.Object{db}, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*databasev1.SQLiteDB); ok {
					dbGetCount++
					if dbGetCount == 1 {
						// First Get (the re-fetch inside resumeReplication) must succeed.
						return c.Get(ctx, k, o, opts...)
					}
				}
				return c.Get(ctx, k, o, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := o.(*databasev1.SQLiteDB); ok {
					return errTransient
				}
				return c.Patch(ctx, o, p, opts...)
			},
		})
		err := r.resumeReplication(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("getTargetWorkloadForRestore returns error for non-NotFound StatefulSet Get failure", func() {
		db := fakeDB(dbName, ns, "")
		db.Spec.TargetDeployment = ""
		db.Spec.TargetStatefulSet = "my-sts"

		r := newFakeRestoreReconciler([]client.Object{db}, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*appsv1.StatefulSet); ok {
					return errTransient
				}
				return c.Get(ctx, k, o, opts...)
			},
		})
		_, err := r.getTargetWorkloadForRestore(ctx, db)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("transient"))
	})

	It("reconcileScalingUp propagates scaleWorkload Patch error", func() {
		db := fakeDB(dbName, ns, depName)
		dep := fakeDeployment(depName, ns)
		restore := fakeRestore(restoreName, ns, dbName, databasev1.RestorePhaseScalingUp)
		// OriginalReplicas=2 so scaleWorkload must Patch (current=1, target=2 → not already at target).
		replicas := int32(2)
		restore.Status.OriginalReplicas = &replicas

		r := newFakeRestoreReconciler([]client.Object{db, dep, restore}, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := o.(*appsv1.Deployment); ok {
					return errTransient
				}
				return c.Patch(ctx, o, p, opts...)
			},
		})
		_, err := r.Reconcile(ctx, restoreKey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("scaling"))
	})
})

// Ensure the notFoundErr helper is used (suppresses unused-variable lint).
var _ = notFoundErr
