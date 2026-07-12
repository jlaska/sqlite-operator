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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

// litestreamContainerName is the name given to the injected sidecar container.
const litestreamContainerName = "litestream"

// litestreamConfigVolume is the name of the volume that mounts litestream.yml.
const litestreamConfigVolume = "litestream-config"

// litestreamConfigMount is the path where the Litestream config is mounted.
const litestreamConfigMount = "/etc/litestream"

// litestreamDefaultImage is the default Litestream container image.
const litestreamDefaultImage = "litestream/litestream:0.5.14"

// injectTrue is the value used for the injection annotation.
const injectTrue = "true"

// archiveCheckContainerName is the name of the archive-check init container.
const archiveCheckContainerName = "litestream-archive-check"

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
	if pod.Annotations[databasev1.AnnotationInject] != injectTrue {
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

// defaultEphemeralStorageLimit is applied to the Litestream sidecar when no
// explicit resource limits are set. Litestream's LTX staging can silently fill
// disk with no error (upstream #1310); this limit surfaces the failure visibly.
const defaultEphemeralStorageLimit = "1Gi"

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
		image = litestreamDefaultImage
	}

	sidecar := corev1.Container{
		Name:  litestreamContainerName,
		Image: image,
		Args:  []string{"replicate", "-config", "/etc/litestream/litestream.yml"},
		Ports: []corev1.ContainerPort{
			{Name: "metrics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      volumeName,
				MountPath: db.Spec.DatabasePath,
			},
			{
				Name:      litestreamConfigVolume,
				MountPath: litestreamConfigMount,
				ReadOnly:  true,
			},
		},
		Resources: litestreamResources(db),
	}

	// Inject S3 credentials and optional log level from the referenced Secret.
	if db.Spec.Backup.Enabled && db.Spec.Backup.Destination.S3 != nil {
		sidecar.Env = s3CredsEnvVars(db.Spec.Backup.Destination.S3.SecretRef)
	}
	if db.Spec.Backup.LogLevel != "" {
		sidecar.Env = append(sidecar.Env, corev1.EnvVar{
			Name:  "LITESTREAM_LOG_LEVEL",
			Value: db.Spec.Backup.LogLevel,
		})
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

	// Add Prometheus scrape annotations to the pod so standard service monitors
	// can discover the sidecar's /metrics endpoint.
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations["prometheus.io/scrape"] = "true"
	pod.Annotations["prometheus.io/port"] = "9090"
	pod.Annotations["prometheus.io/path"] = "/metrics"

	// Inject the startup init container:
	//   autoRestore=true  → upstream-style restore with mandatory integrity gate
	//   autoRestore=false → archive-check that blocks if S3 has data but DB missing
	if db.Spec.Backup.Enabled {
		skipArchive := db.Annotations[databasev1.AnnotationSkipArchiveCheck] == "true"
		if db.Spec.Backup.AutoRestore {
			s.injectAutoRestoreContainer(pod, db, volumeName)
		} else if !skipArchive {
			s.injectArchiveCheckContainer(pod, db, volumeName)
		}
	}

	// Inject the SQL init container when InitSQL is configured.
	if db.Spec.InitSQL != "" {
		s.injectInitContainer(pod, db, volumeName)
	}

	return nil
}

// litestreamResources returns the resource requirements for the Litestream sidecar.
// When the user has not specified resources, a default ephemeral-storage limit is
// applied to surface the silent disk-fill failure mode (upstream #1310).
func litestreamResources(db *databasev1.SQLiteDB) corev1.ResourceRequirements {
	if db.Spec.Backup.Resources != nil {
		return *db.Spec.Backup.Resources
	}
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceEphemeralStorage: resource.MustParse(defaultEphemeralStorageLimit),
		},
	}
}

// autoRestoreContainerName is the name of the auto-restore init container.
const autoRestoreContainerName = "litestream-restore"

// buildLitestreamInitContainer builds the shared container structure for both
// the archive-check and auto-restore init containers. Both containers use the
// same image, env vars, and volume mounts; only the name and script differ.
func buildLitestreamInitContainer(name, script, image, dbPath, dataVolumeName string, envVars []corev1.EnvVar) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   image,
		Command: []string{"sh", "-c", script},
		Env:     envVars,
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dbPath},
			{Name: litestreamConfigVolume, MountPath: litestreamConfigMount, ReadOnly: true},
		},
	}
}

// injectArchiveCheckContainer injects an init container that checks whether the
// DB file exists on the PVC. If the DB is missing but S3 already has backup data,
// the container exits non-zero, blocking pod startup and preventing Litestream from
// creating a new snapshot chain over the existing backup. This mirrors CNPG's
// "empty WAL archive check" pattern.
//
// The check runs before the app starts, so there is no race with app DB initialization.
func (s *SidecarInjector) injectArchiveCheckContainer(pod *corev1.Pod, db *databasev1.SQLiteDB, dataVolumeName string) {
	image := db.Spec.Image
	if image == "" {
		image = litestreamDefaultImage
	}

	dbFullPath := db.Spec.DatabasePath + "/" + db.Spec.DatabaseName

	// Shell script logic:
	//   1. If the DB file exists → pass (normal restart or first-time setup with pre-existing DB).
	//   2. If DB is missing AND S3 has restorable data → fail with actionable message.
	//   3. If DB is missing AND S3 is empty → pass (first-time setup).
	//
	// Uses `litestream restore` as the S3 probe instead of `litestream snapshots`
	// because in v0.5.x `snapshots` is an IPC command that requires a running daemon;
	// it always returns empty when invoked standalone in a one-off init container.
	// `litestream restore` works standalone and exits 0 only when restorable data exists.
	script := fmt.Sprintf(`
DB_PATH="%s"
if [ -f "${DB_PATH}" ]; then
  echo "archive-check: database file exists, skipping check"
  exit 0
fi
echo "archive-check: database file missing at ${DB_PATH}, probing S3 for backup data..."
PROBE="${DB_PATH}.archive-check-probe"
rm -f "${PROBE}"
if litestream restore -config /etc/litestream/litestream.yml -o "${PROBE}" "${DB_PATH}" 2>/dev/null; then
  rm -f "${PROBE}"
  echo "archive-check FAILED: S3 has existing backup data but local database is missing."
  echo "This likely means data was lost (PVC wiped or DB deleted)."
  echo "To recover: create a SQLiteRestore CR targeting this PVC."
  echo "To bypass (start fresh): set annotation sqlite.database.example.com/skip-archive-check=true"
  exit 1
fi
rm -f "${PROBE}"
echo "archive-check: no S3 backup found, safe to proceed (first-time setup)"
exit 0
`, dbFullPath)

	envVars := []corev1.EnvVar{}
	if db.Spec.Backup.Destination.S3 != nil {
		envVars = s3CredsEnvVars(db.Spec.Backup.Destination.S3.SecretRef)
	}

	c := buildLitestreamInitContainer(archiveCheckContainerName, script, image, db.Spec.DatabasePath, dataVolumeName, envVars)
	pod.Spec.InitContainers = append([]corev1.Container{c}, pod.Spec.InitContainers...)
}

// injectAutoRestoreContainer adds an init container that implements the upstream
// Kubernetes guide's recommended auto-restore pattern:
//  1. litestream restore -if-db-not-exists -if-replica-exists (restore if missing + backup exists)
//  2. PRAGMA quick_check integrity gate (blocks pod startup on corrupt restore)
//
// This replaces the archive-check container when spec.backup.autoRestore=true.
// The integrity gate mitigates known Litestream restore corruption issues (#1164, #1220).
func (s *SidecarInjector) injectAutoRestoreContainer(pod *corev1.Pod, db *databasev1.SQLiteDB, dataVolumeName string) {
	image := db.Spec.Image
	if image == "" {
		image = litestreamDefaultImage
	}

	dbFullPath := db.Spec.DatabasePath + "/" + db.Spec.DatabaseName

	// The script:
	//   1. Restore with upstream flags — skips if DB exists or no backup available.
	//   2. If restore ran (DB now present), validate integrity with sqlite3.
	//   3. On integrity failure, block pod startup with an actionable error.
	script := fmt.Sprintf(`
DB_PATH="%s"
if [ -f "${DB_PATH}" ]; then
  echo "litestream-restore: database exists, skipping restore"
  exit 0
fi
echo "litestream-restore: database missing, attempting restore from backup..."
litestream restore -if-db-not-exists -if-replica-exists -config /etc/litestream/litestream.yml "${DB_PATH}"
RESTORE_EXIT=$?
if [ $RESTORE_EXIT -ne 0 ]; then
  echo "litestream-restore: restore failed or no backup found, starting fresh"
  exit 0
fi
if [ ! -f "${DB_PATH}" ]; then
  echo "litestream-restore: no backup found, starting fresh"
  exit 0
fi
echo "litestream-restore: restore complete, running integrity check..."
if ! sqlite3 "${DB_PATH}" "PRAGMA quick_check;" | grep -q "^ok$"; then
  echo "ERROR: integrity check failed on restored database."
  echo "The S3 backup may contain corruption (Litestream upstream issue #1164/#1220)."
  echo "Options:"
  echo "  1. Use a SQLiteRestore CR with a different -timestamp to find a clean snapshot."
  echo "  2. Set annotation sqlite.database.example.com/skip-archive-check=true to start fresh."
  exit 1
fi
echo "litestream-restore: integrity check passed"
exit 0
`, dbFullPath)

	envVars := []corev1.EnvVar{}
	if db.Spec.Backup.Destination.S3 != nil {
		envVars = s3CredsEnvVars(db.Spec.Backup.Destination.S3.SecretRef)
	}

	c := buildLitestreamInitContainer(autoRestoreContainerName, script, image, db.Spec.DatabasePath, dataVolumeName, envVars)
	pod.Spec.InitContainers = append([]corev1.Container{c}, pod.Spec.InitContainers...)
}

// s3CredsEnvVars builds S3 credential env vars from a Secret reference.
func s3CredsEnvVars(secretRef string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name: "LITESTREAM_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretRef},
					Key:                  "ACCESS_KEY_ID",
				},
			},
		},
		{
			Name: "LITESTREAM_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretRef},
					Key:                  "SECRET_ACCESS_KEY",
				},
			},
		},
	}
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
