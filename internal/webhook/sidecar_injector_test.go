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

package webhook_test

import (
	"context"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
	"github.com/jlaska/sqlite-operator/internal/webhook"
)

var _ = Describe("SidecarInjector", func() {
	const (
		namespace         = "default"
		sqliteDBName      = "test-db"
		deploymentName    = "test-app"
		databaseName      = "myapp.db"
		databasePath      = "/data"
		volumeName        = "app-data"
		appContainerName  = "app"     // goconst
		appContainerImage = "busybox" // goconst
		litestreamName    = "litestream" // goconst
		injectTrue        = "true"    // goconst
	)

	ctx := context.Background()

	// Helper: build a SQLiteDB CR.
	newSQLiteDB := func(backupEnabled bool) *databasev1.SQLiteDB {
		db := &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sqliteDBName,
				Namespace: namespace,
			},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     databaseName,
				DatabasePath:     databasePath,
				TargetDeployment: deploymentName,
			},
		}
		if backupEnabled {
			db.Spec.Backup = databasev1.BackupSpec{
				Enabled: true,
				Destination: databasev1.BackupDestination{
					S3: &databasev1.S3Destination{
						Bucket:    "my-bucket",
						SecretRef: "s3-creds",
					},
				},
			}
		}
		return db
	}

	// Helper: build a pod with the inject annotation and a volume at databasePath.
	newAnnotatedPod := func(configRef string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: namespace,
				Annotations: map[string]string{
					databasev1.AnnotationInject: injectTrue,
					databasev1.AnnotationConfig: configRef,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  appContainerName,
						Image: appContainerImage,
						VolumeMounts: []corev1.VolumeMount{
							{Name: volumeName, MountPath: databasePath},
						},
					},
				},
				Volumes: []corev1.Volume{
					{Name: volumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		}
	}

	// Helper: build an admission.Request for a pod.
	makeRequest := func(pod *corev1.Pod) admission.Request {
		raw, err := json.Marshal(pod)
		Expect(err).NotTo(HaveOccurred())
		return admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				UID:       types.UID("test-uid"),
				Namespace: namespace,
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
			},
		}
	}

	// Helper: build an injector backed by the envtest client.
	newInjector := func() *webhook.SidecarInjector {
		return &webhook.SidecarInjector{
			Client:  k8sClient,
			Decoder: admission.NewDecoder(k8sClient.Scheme()),
		}
	}

	BeforeEach(func() {
		db := newSQLiteDB(false)
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
	})

	AfterEach(func() {
		db := &databasev1.SQLiteDB{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: sqliteDBName, Namespace: namespace}, db)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, db)).To(Succeed())
	})

	It("allows pods without the injection annotation", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "plain-pod", Namespace: namespace},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: appContainerImage}},
			},
		}
		resp := newInjector().Handle(ctx, makeRequest(pod))
		Expect(resp.Allowed).To(BeTrue())
		Expect(resp.Patches).To(BeEmpty())
	})

	It("injects the Litestream sidecar into annotated pods", func() {
		pod := newAnnotatedPod(namespace + "/" + sqliteDBName)
		resp := newInjector().Handle(ctx, makeRequest(pod))
		Expect(resp.Allowed).To(BeTrue())

		// Apply the JSON patch and inspect the resulting pod.
		patched := applyPatches(pod, resp.Patches)
		containerNames := make([]string, len(patched.Spec.Containers))
		for i, c := range patched.Spec.Containers {
			containerNames[i] = c.Name
		}
		Expect(containerNames).To(ContainElement(litestreamName))

		// The sidecar must mount the same data volume.
		var sidecar corev1.Container
		for _, c := range patched.Spec.Containers {
			if c.Name == litestreamName {
				sidecar = c
				break
			}
		}
		mountPaths := make([]string, len(sidecar.VolumeMounts))
		for i, vm := range sidecar.VolumeMounts {
			mountPaths[i] = vm.MountPath
		}
		Expect(mountPaths).To(ContainElement(databasePath))
		Expect(mountPaths).To(ContainElement("/etc/litestream"))
	})

	It("is idempotent — does not inject twice", func() {
		pod := newAnnotatedPod(namespace + "/" + sqliteDBName)
		injector := newInjector()

		first := injector.Handle(ctx, makeRequest(pod))
		Expect(first.Allowed).To(BeTrue())

		// Simulate the pod already having the sidecar injected.
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name: litestreamName, Image: "litestream/litestream:0.3.13",
		})
		second := injector.Handle(ctx, makeRequest(pod))
		Expect(second.Allowed).To(BeTrue())
		Expect(second.Patches).To(BeEmpty())
	})

	It("injects S3 credential env vars when backup is enabled", func() {
		// Replace the CR with one that has backup enabled.
		db := &databasev1.SQLiteDB{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sqliteDBName, Namespace: namespace}, db)).To(Succeed())
		Expect(k8sClient.Delete(ctx, db)).To(Succeed())

		Expect(k8sClient.Create(ctx, newSQLiteDB(true))).To(Succeed())

		pod := newAnnotatedPod(namespace + "/" + sqliteDBName)
		resp := newInjector().Handle(ctx, makeRequest(pod))
		Expect(resp.Allowed).To(BeTrue())

		patched := applyPatches(pod, resp.Patches)
		var sidecar corev1.Container
		for _, c := range patched.Spec.Containers {
			if c.Name == litestreamName {
				sidecar = c
				break
			}
		}
		envNames := make([]string, len(sidecar.Env))
		for i, e := range sidecar.Env {
			envNames[i] = e.Name
		}
		Expect(envNames).To(ContainElements("LITESTREAM_ACCESS_KEY_ID", "LITESTREAM_SECRET_ACCESS_KEY"))
	})
})

var _ = Describe("SidecarInjector init container", func() {
	const (
		namespace    = "default"
		sqliteDBName = "init-test-db"
		deployName   = "init-test-app"
		databaseName = "app.db"
		databasePath = "/data"
		volumeName   = "app-data"
		initSQL           = "CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY);"
		injectTrue        = "true"    // goconst: mirrors constant in first Describe
		appContainerName  = "app"     // goconst: mirrors constant in first Describe
		appContainerImage = "busybox" // goconst: mirrors constant in first Describe
	)

	ctx := context.Background()

	newInjector := func() *webhook.SidecarInjector {
		return &webhook.SidecarInjector{
			Client:  k8sClient,
			Decoder: admission.NewDecoder(k8sClient.Scheme()),
		}
	}

	newPod := func(annotations map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-pod", Namespace: namespace,
				Annotations: annotations,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "app", Image: appContainerImage,
					VolumeMounts: []corev1.VolumeMount{{Name: volumeName, MountPath: databasePath}},
				}},
				Volumes: []corev1.Volume{{
					Name:         volumeName,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				}},
			},
		}
	}

	makePodRequest := func(pod *corev1.Pod) admission.Request {
		raw, err := json.Marshal(pod)
		Expect(err).NotTo(HaveOccurred())
		return admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				UID: "test-uid", Namespace: namespace,
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
			},
		}
	}

	BeforeEach(func() {
		db := &databasev1.SQLiteDB{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: sqliteDBName, Namespace: namespace}, db); err != nil {
			db = &databasev1.SQLiteDB{
				ObjectMeta: metav1.ObjectMeta{Name: sqliteDBName, Namespace: namespace},
				Spec: databasev1.SQLiteDBSpec{
					DatabaseName:     databaseName,
					DatabasePath:     databasePath,
					TargetDeployment: deployName,
					InitSQL:          initSQL,
				},
			}
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
		}
	})

	AfterEach(func() {
		db := &databasev1.SQLiteDB{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: sqliteDBName, Namespace: namespace}, db); err == nil {
			Expect(k8sClient.Delete(ctx, db)).To(Succeed())
		}
	})

	It("injects an init container when InitSQL is set", func() {
		annotations := map[string]string{
			databasev1.AnnotationInject: injectTrue,
			databasev1.AnnotationConfig: namespace + "/" + sqliteDBName,
		}
		pod := newPod(annotations)
		resp := newInjector().Handle(ctx, makePodRequest(pod))
		Expect(resp.Allowed).To(BeTrue())

		patched := applyInitPatches(pod, resp.Patches)
		initNames := make([]string, len(patched.Spec.InitContainers))
		for i, c := range patched.Spec.InitContainers {
			initNames[i] = c.Name
		}
		Expect(initNames).To(ContainElement("sqlite-init"))
	})

	It("init container script references the correct database path", func() {
		annotations := map[string]string{
			databasev1.AnnotationInject: injectTrue,
			databasev1.AnnotationConfig: namespace + "/" + sqliteDBName,
		}
		pod := newPod(annotations)
		resp := newInjector().Handle(ctx, makePodRequest(pod))
		Expect(resp.Allowed).To(BeTrue())

		patched := applyInitPatches(pod, resp.Patches)
		var initContainer corev1.Container
		for _, c := range patched.Spec.InitContainers {
			if c.Name == "sqlite-init" {
				initContainer = c
				break
			}
		}
		Expect(initContainer.Command).To(ContainElement(ContainSubstring(databasePath + "/" + databaseName)))
	})

	It("does not inject an init container when InitSQL is empty", func() {
		// Create a second DB with no initSQL.
		noInitDB := &databasev1.SQLiteDB{
			ObjectMeta: metav1.ObjectMeta{Name: "no-init-db", Namespace: namespace},
			Spec: databasev1.SQLiteDBSpec{
				DatabaseName:     databaseName,
				DatabasePath:     databasePath,
				TargetDeployment: deployName,
			},
		}
		Expect(k8sClient.Create(ctx, noInitDB)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, noInitDB) }()

		annotations := map[string]string{
			databasev1.AnnotationInject: injectTrue,
			databasev1.AnnotationConfig: namespace + "/no-init-db",
		}
		pod := newPod(annotations)
		resp := newInjector().Handle(ctx, makePodRequest(pod))
		Expect(resp.Allowed).To(BeTrue())

		patched := applyInitPatches(pod, resp.Patches)
		Expect(patched.Spec.InitContainers).To(BeEmpty())
	})
})

// applyInitPatches reconstructs the mutated pod from JSON-patch add operations
// produced by admission.PatchResponseFromRaw. The path for appended array
// elements uses a numeric index (e.g. /spec/containers/1), so we match on
// the path prefix rather than a literal "/-" suffix.
// applyPatches / applyInitPatches reconstruct the mutated pod from JSON-patch
// "add" operations produced by admission.PatchResponseFromRaw.
func applyPatches(pod *corev1.Pod, patches []jsonpatch.JsonPatchOperation) *corev1.Pod {
	return applyAllPatches(pod, patches)
}

func applyInitPatches(pod *corev1.Pod, patches []jsonpatch.JsonPatchOperation) *corev1.Pod {
	return applyAllPatches(pod, patches)
}

func applyAllPatches(pod *corev1.Pod, patches []jsonpatch.JsonPatchOperation) *corev1.Pod {
	if len(patches) == 0 {
		return pod
	}
	out := pod.DeepCopy()
	for _, op := range patches {
		if op.Operation != "add" {
			continue
		}
		raw, err := json.Marshal(op.Value)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(op.Path, "/spec/containers/") || op.Path == "/spec/containers/-":
			var c corev1.Container
			if err := json.Unmarshal(raw, &c); err == nil && c.Name != "" {
				out.Spec.Containers = append(out.Spec.Containers, c)
			}
		case op.Path == "/spec/initContainers":
			// Whole array added (field was null before injection).
			var cs []corev1.Container
			if err := json.Unmarshal(raw, &cs); err == nil {
				out.Spec.InitContainers = append(out.Spec.InitContainers, cs...)
			}
		case strings.HasPrefix(op.Path, "/spec/initContainers/") || op.Path == "/spec/initContainers/-":
			var c corev1.Container
			if err := json.Unmarshal(raw, &c); err == nil && c.Name != "" {
				out.Spec.InitContainers = append(out.Spec.InitContainers, c)
			}
		case strings.HasPrefix(op.Path, "/spec/volumes/") || op.Path == "/spec/volumes/-":
			var v corev1.Volume
			if err := json.Unmarshal(raw, &v); err == nil && v.Name != "" {
				out.Spec.Volumes = append(out.Spec.Volumes, v)
			}
		}
	}
	return out
}
