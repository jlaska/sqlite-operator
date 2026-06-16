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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	)

	ctx := context.Background()
	namespacedName := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
	deploymentKey := types.NamespacedName{Name: deploymentName, Namespace: namespaceName}

	BeforeEach(func() {
		// Create a target Deployment for the controller to annotate.
		dep := &appsv1.Deployment{}
		err := k8sClient.Get(ctx, deploymentKey, dep)
		if err != nil && errors.IsNotFound(err) {
			dep = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: namespaceName,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": deploymentName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": deploymentName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "app", Image: "busybox"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		}

		// Create the SQLiteDB CR.
		db := &databasev1.SQLiteDB{}
		err = k8sClient.Get(ctx, namespacedName, db)
		if err != nil && errors.IsNotFound(err) {
			db = &databasev1.SQLiteDB{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
				},
				Spec: databasev1.SQLiteDBSpec{
					DatabaseName:     databaseName,
					DatabasePath:     databasePath,
					TargetDeployment: deploymentName,
					Backup: databasev1.BackupSpec{
						Enabled: false,
					},
				},
			}
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
		}
	})

	AfterEach(func() {
		db := &databasev1.SQLiteDB{}
		err := k8sClient.Get(ctx, namespacedName, db)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, db)).To(Succeed())

		dep := &appsv1.Deployment{}
		err = k8sClient.Get(ctx, deploymentKey, dep)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
	})

	It("should reconcile without error", func() {
		reconciler := &SQLiteDBReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())
	})

	It("should create the Litestream ConfigMap", func() {
		reconciler := &SQLiteDBReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		err = k8sClient.Get(ctx, types.NamespacedName{
			Name:      resourceName + "-litestream",
			Namespace: namespaceName,
		}, cm)
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data).To(HaveKey("litestream.yml"))
		Expect(cm.Data["litestream.yml"]).To(ContainSubstring(databasePath + "/" + databaseName))
	})

	It("should annotate the target Deployment", func() {
		reconciler := &SQLiteDBReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deploymentKey, dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKeyWithValue(injectAnnotation, "true"))
		Expect(dep.Annotations).To(HaveKey(configAnnotation))
	})
})
