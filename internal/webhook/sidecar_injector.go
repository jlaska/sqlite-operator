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

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

// litestreamContainerName is the name given to the injected sidecar container.
const litestreamContainerName = "litestream"

// litestreamConfigVolume is the name of the volume that mounts litestream.yml.
const litestreamConfigVolume = "litestream-config"

// sqliteInitContainerName is the name given to the injected init container.
const sqliteInitContainerName = "sqlite-init"

// sqliteInitSQLVolume is the name of the volume that mounts init.sql.
const sqliteInitSQLVolume = "sqlite-init-sql"

// SidecarInjector is a mutating admission webhook that injects a Litestream
// replication sidecar into pods belonging to annotated Deployments.
//
// It is registered as a raw admission.Handler (not a typed CRD webhook) because
// it operates on core/v1 Pod resources.
type SidecarInjector struct {
	Client  client.Client
	Decoder admission.Decoder
}

// Handle processes an admission request for a Pod and injects the Litestream
// sidecar when the pod carries the injection annotation.
func (s *SidecarInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := s.Decoder.DecodeRaw(req.Object, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding pod: %w", err))
	}

	// Only act on pods that carry the injection annotation.
	if pod.Annotations[databasev1.AnnotationInject] != "true" {
		return admission.Allowed("no injection annotation")
	}

	// Skip if already injected (idempotency guard).
	if s.alreadyInjected(pod) {
		return admission.Allowed("sidecar already present")
	}

	// Resolve the SQLiteDB CR from the config annotation.
	sqliteDB, err := s.resolveSQLiteDB(ctx, pod, req.Namespace)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if sqliteDB == nil {
		return admission.Allowed("no SQLiteDB config reference found")
	}

	// Inject the sidecar and return the patch.
	if err := s.inject(pod, sqliteDB); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("injecting sidecar: %w", err))
	}

	marshalled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshalling patched pod: %w", err))
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshalled)
}

// alreadyInjected returns true if the Litestream container is already present,
// preventing duplicate injection on pod updates or retries.
func (s *SidecarInjector) alreadyInjected(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == litestreamContainerName {
			return true
		}
	}
	return false
}

// resolveSQLiteDB looks up the SQLiteDB CR referenced by the config annotation.
// The annotation value is "namespace/name". Returns nil (no error) when the
// annotation is absent.
func (s *SidecarInjector) resolveSQLiteDB(ctx context.Context, pod *corev1.Pod, podNamespace string) (*databasev1.SQLiteDB, error) {
	ref := pod.Annotations[databasev1.AnnotationConfig]
	if ref == "" {
		return nil, nil
	}

	ns, name, found := strings.Cut(ref, "/")
	if !found {
		return nil, fmt.Errorf("malformed %s annotation %q: expected namespace/name", databasev1.AnnotationConfig, ref)
	}
	if ns == "" {
		ns = podNamespace
	}

	db := &databasev1.SQLiteDB{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, db); err != nil {
		return nil, fmt.Errorf("getting SQLiteDB %s/%s: %w", ns, name, err)
	}
	return db, nil
}

// inject mutates the pod spec in-place to add the Litestream sidecar.
func (s *SidecarInjector) inject(pod *corev1.Pod, db *databasev1.SQLiteDB) error {
	// The sidecar shares the volume that already mounts the database path.
	// We look for a volume mount in the first container that covers databasePath.
	volumeName, err := s.findVolumeForPath(pod, db.Spec.DatabasePath)
	if err != nil {
		return err
	}

	image := db.Spec.Image
	if image == "" {
		image = "litestream/litestream:0.3.13"
	}

	sidecar := corev1.Container{
		Name:  litestreamContainerName,
		Image: image,
		Args:  []string{"replicate", "-config", "/etc/litestream/litestream.yml"},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      volumeName,
				MountPath: db.Spec.DatabasePath,
			},
			{
				Name:      litestreamConfigVolume,
				MountPath: "/etc/litestream",
				ReadOnly:  true,
			},
		},
	}

	// Inject S3 credentials as environment variables from the referenced Secret.
	if db.Spec.Backup.Enabled && db.Spec.Backup.Destination.S3 != nil {
		secretRef := db.Spec.Backup.Destination.S3.SecretRef
		sidecar.Env = []corev1.EnvVar{
			{
				Name: "LITESTREAM_ACCESS_KEY_ID",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretRef},
						Key:                  "access-key-id",
					},
				},
			},
			{
				Name: "LITESTREAM_SECRET_ACCESS_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretRef},
						Key:                  "secret-access-key",
					},
				},
			},
		}
	}

	pod.Spec.Containers = append(pod.Spec.Containers, sidecar)

	// Add the ConfigMap volume for litestream.yml (only once).
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: litestreamConfigVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: db.Name + "-litestream",
				},
			},
		},
	})

	// Inject the init container when InitSQL is configured.
	if db.Spec.InitSQL != "" {
		s.injectInitContainer(pod, db, volumeName)
	}

	return nil
}

// injectInitContainer adds an init container that applies InitSQL to the
// database exactly once, guarded by a SHA-256 hash marker file on the PVC.
// The marker file lives at {databasePath}/.sqlite-init-{hash} so it persists
// across pod restarts; a change in InitSQL produces a new hash and a new
// marker, triggering re-application on the next rollout.
func (s *SidecarInjector) injectInitContainer(pod *corev1.Pod, db *databasev1.SQLiteDB, dataVolumeName string) {
	initImage := db.Spec.InitImage
	if initImage == "" {
		initImage = "keinos/sqlite3:latest"
	}

	dbFullPath := db.Spec.DatabasePath + "/" + db.Spec.DatabaseName

	// The shell script runs inside the init container:
	//   1. Compute the SHA-256 hash of init.sql.
	//   2. If the hash marker file does not exist, apply the SQL and create it.
	//   3. Exit 0 in both cases so pod startup is never blocked by a prior run.
	script := fmt.Sprintf(`
HASH=$(sha256sum /init/init.sql | cut -d' ' -f1)
MARKER="%s/.sqlite-init-${HASH}"
if [ ! -f "${MARKER}" ]; then
  sqlite3 "%s" < /init/init.sql
  touch "${MARKER}"
  echo "sqlite-init: applied init SQL (hash ${HASH})"
else
  echo "sqlite-init: already applied (hash ${HASH}), skipping"
fi
`, db.Spec.DatabasePath, dbFullPath)

	initContainer := corev1.Container{
		Name:    sqliteInitContainerName,
		Image:   initImage,
		Command: []string{"sh", "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      dataVolumeName,
				MountPath: db.Spec.DatabasePath,
			},
			{
				Name:      sqliteInitSQLVolume,
				MountPath: "/init",
				ReadOnly:  true,
			},
		},
	}

	pod.Spec.InitContainers = append(pod.Spec.InitContainers, initContainer)

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: sqliteInitSQLVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: db.Name + "-init-sql",
				},
			},
		},
	})
}

// findVolumeForPath returns the name of a volume whose mount path in the first
// application container covers the given database path. Returns an error if
// none is found — the operator requires the app to mount its data volume
// explicitly so that Litestream can share it.
func (s *SidecarInjector) findVolumeForPath(pod *corev1.Pod, dbPath string) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("pod has no containers")
	}

	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.MountPath == dbPath || strings.HasPrefix(dbPath, vm.MountPath+"/") {
			return vm.Name, nil
		}
	}

	return "", fmt.Errorf(
		"no volume mount in container %q covers database path %q; "+
			"ensure the application mounts a volume at %q",
		pod.Spec.Containers[0].Name, dbPath, dbPath,
	)
}
