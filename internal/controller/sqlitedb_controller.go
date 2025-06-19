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

// Package controller contains the SQLite database controller implementation.
package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

// SQLiteDBReconciler reconciles a SQLiteDB object
type SQLiteDBReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.example.com,resources=sqlitedbs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SQLiteDB object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *SQLiteDBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the SQLiteDB instance
	sqliteDB := &databasev1.SQLiteDB{}
	if err := r.Get(ctx, req.NamespacedName, sqliteDB); err != nil {
		if errors.IsNotFound(err) {
			log.Info("SQLiteDB resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get SQLiteDB")
		return ctrl.Result{}, err
	}

	// Create or update PVC
	if err := r.reconcilePVC(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile PVC")
		return ctrl.Result{}, err
	}

	// Create or update ConfigMap for init SQL
	if err := r.reconcileConfigMap(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}

	// Create or update Deployment
	if err := r.reconcileDeployment(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile Deployment")
		return ctrl.Result{}, err
	}

	// Create or update Service
	if err := r.reconcileService(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to reconcile Service")
		return ctrl.Result{}, err
	}

	// Update status
	if err := r.updateStatus(ctx, sqliteDB); err != nil {
		log.Error(err, "Failed to update SQLiteDB status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SQLiteDBReconciler) reconcilePVC(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sqliteDB.Name + "-storage",
			Namespace: sqliteDB.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		storageSize := "1Gi"
		if sqliteDB.Spec.StorageSize != "" {
			storageSize = sqliteDB.Spec.StorageSize
		}

		pvc.Spec = corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(storageSize),
				},
			},
		}
		return controllerutil.SetControllerReference(sqliteDB, pvc, r.Scheme)
	})

	return err
}

func (r *SQLiteDBReconciler) reconcileConfigMap(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	if sqliteDB.Spec.InitSQL == "" {
		return nil
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sqliteDB.Name + "-init",
			Namespace: sqliteDB.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"init.sql": sqliteDB.Spec.InitSQL,
		}
		return controllerutil.SetControllerReference(sqliteDB, cm, r.Scheme)
	})

	return err
}

func (r *SQLiteDBReconciler) reconcileDeployment(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sqliteDB.Name,
			Namespace: sqliteDB.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		replicas := int32(1)
		if sqliteDB.Spec.Replicas != nil {
			replicas = *sqliteDB.Spec.Replicas
		}

		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": sqliteDB.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": sqliteDB.Name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "sqlite",
							Image: "keinos/sqlite3:latest",
							Command: []string{
								"sh", "-c",
								fmt.Sprintf("sqlite3 /data/%s.db < /init/init.sql || true; sqlite3 /data/%s.db '.timeout 30000' '.backup /data/%s.db'",
									sqliteDB.Spec.DatabaseName, sqliteDB.Spec.DatabaseName, sqliteDB.Spec.DatabaseName),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "storage",
									MountPath: "/data",
								},
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 8080,
									Protocol:      corev1.ProtocolTCP,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "storage",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: sqliteDB.Name + "-storage",
								},
							},
						},
					},
				},
			},
		}

		if sqliteDB.Spec.InitSQL != "" {
			deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "init-sql",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: sqliteDB.Name + "-init",
						},
					},
				},
			})
			deployment.Spec.Template.Spec.Containers[0].VolumeMounts = append(
				deployment.Spec.Template.Spec.Containers[0].VolumeMounts,
				corev1.VolumeMount{
					Name:      "init-sql",
					MountPath: "/init",
				},
			)
		}

		return controllerutil.SetControllerReference(sqliteDB, deployment, r.Scheme)
	})

	return err
}

func (r *SQLiteDBReconciler) reconcileService(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sqliteDB.Name + "-service",
			Namespace: sqliteDB.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Spec = corev1.ServiceSpec{
			Selector: map[string]string{
				"app": sqliteDB.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       8080,
					TargetPort: intstr.FromInt32(8080),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		}
		return controllerutil.SetControllerReference(sqliteDB, service, r.Scheme)
	})

	return err
}

func (r *SQLiteDBReconciler) updateStatus(ctx context.Context, sqliteDB *databasev1.SQLiteDB) error {
	// Get the deployment to check status
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: sqliteDB.Namespace,
		Name:      sqliteDB.Name,
	}, deployment)

	if err != nil {
		sqliteDB.Status.Phase = "Creating"
		sqliteDB.Status.Ready = false
	} else {
		if deployment.Status.ReadyReplicas > 0 {
			sqliteDB.Status.Phase = "Ready"
			sqliteDB.Status.Ready = true
		} else {
			sqliteDB.Status.Phase = "Pending"
			sqliteDB.Status.Ready = false
		}
	}

	return r.Status().Update(ctx, sqliteDB)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SQLiteDBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.SQLiteDB{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Named("sqlitedb").
		Complete(r)
}
