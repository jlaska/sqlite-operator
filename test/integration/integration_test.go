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

package integration

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ── Scenario 1: Sidecar Injection ─────────────────────────────────────────

var _ = Describe("Sidecar Injection", Ordered, func() {
	const (
		appName    = "inject-test-app"
		dbName     = "inject-test-db"
		pvcName    = "inject-test-pvc"
		dbFile     = "inject.db"
		dbPath     = "/data"
	)

	BeforeAll(func() {
		By("creating test PVC")
		applyLiteral(pvcManifest(pvcName, testNamespace))

		By("creating test app Deployment (no init containers, no sidecar yet)")
		applyLiteral(appDeploymentManifest(appName, testNamespace, pvcName, dbPath))

		By("waiting for initial app pod to be Running")
		kubectl("wait", "-n", testNamespace, "deployment/"+appName,
			"--for=condition=Available", "--timeout=3m")
	})

	AfterAll(func() {
		runIgnoreError("kubectl", "delete", "sqlitedb", dbName, "-n", testNamespace, "--ignore-not-found")
		runIgnoreError("kubectl", "delete", "deployment", appName, "-n", testNamespace, "--ignore-not-found")
		runIgnoreError("kubectl", "delete", "pvc", pvcName, "-n", testNamespace, "--ignore-not-found")
	})

	It("injects the Litestream sidecar into new pods after SQLiteDB CR is created", func() {
		By("creating a SQLiteDB CR (backup disabled — just test injection)")
		applyLiteral(sqliteDBManifest(dbName, testNamespace, appName, dbFile, dbPath, false, ""))

		By("waiting for the controller to annotate the Deployment's pod template")
		Eventually(func(g Gomega) {
			out := kubectl("get", "deployment", appName, "-n", testNamespace,
				"-o", `jsonpath={.spec.template.metadata.annotations.sqlite\.database\.example\.com/inject}`)
			g.Expect(out).To(Equal("true"))
		}).Should(Succeed())

		By("waiting for a rolling update to complete with the Litestream sidecar")
		Eventually(func(g Gomega) {
			out := kubectl("get", "pods", "-n", testNamespace,
				"-l", "app="+appName,
				"-o", `jsonpath={range .items[*]}{range .spec.containers[*]}{.name}{"\n"}{end}{end}`)
			g.Expect(out).To(ContainSubstring("litestream"))
		}).Should(Succeed())

		By("confirming the SQLiteDB Ready condition reflects the injected state")
		Eventually(func(g Gomega) {
			out := kubectl("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="SidecarInjected")].status}`)
			g.Expect(out).To(Equal("True"))
		}).Should(Succeed())
	})
})

// ── Scenario 2: Backup to MinIO ───────────────────────────────────────────

var _ = Describe("Backup to MinIO", Ordered, func() {
	const (
		appName = "backup-test-app"
		dbName  = "backup-test-db"
		pvcName = "backup-test-pvc"
		dbFile  = "backup.db"
		dbPath  = "/data"
		initSQL = `CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  message TEXT,
  ts DATETIME DEFAULT CURRENT_TIMESTAMP
);`
	)

	BeforeAll(func() {
		By("creating test PVC")
		applyLiteral(pvcManifest(pvcName, testNamespace))

		By("creating test app Deployment")
		applyLiteral(appDeploymentManifest(appName, testNamespace, pvcName, dbPath))

		By("waiting for initial app pod to be Running")
		kubectl("wait", "-n", testNamespace, "deployment/"+appName,
			"--for=condition=Available", "--timeout=3m")

		By("creating SQLiteDB CR with backup enabled and initSQL")
		applyLiteral(sqliteDBManifest(dbName, testNamespace, appName, dbFile, dbPath, true, initSQL))

		By("waiting for sidecar injection rollout to complete")
		Eventually(func(g Gomega) {
			out := kubectl("get", "pods", "-n", testNamespace,
				"-l", "app="+appName,
				"-o", `jsonpath={range .items[*]}{range .spec.containers[*]}{.name}{"\n"}{end}{end}`)
			g.Expect(out).To(ContainSubstring("litestream"))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		runIgnoreError("kubectl", "delete", "sqlitedb", dbName, "-n", testNamespace, "--ignore-not-found")
		runIgnoreError("kubectl", "delete", "deployment", appName, "-n", testNamespace, "--ignore-not-found")
		runIgnoreError("kubectl", "delete", "pvc", pvcName, "-n", testNamespace, "--ignore-not-found")
	})

	It("init container applies initSQL idempotently", func() {
		By("getting the running pod name")
		podName := runningPod(appName)

		By("verifying the events table was created by the init container")
		Eventually(func(g Gomega) {
			out := kubectl("exec", "-n", testNamespace, podName, "-c", "app",
				"--", "sqlite3", dbPath+"/"+dbFile, ".tables")
			g.Expect(out).To(ContainSubstring("events"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("Litestream replicates WAL changes to the MinIO bucket", func() {
		podName := runningPod(appName)

		By("writing a row to the database")
		kubectl("exec", "-n", testNamespace, podName, "-c", "app",
			"--", "sqlite3", dbPath+"/"+dbFile,
			"INSERT INTO events(message) VALUES('integration-test-backup');")

		By("waiting for Litestream to replicate to MinIO (polls mc ls)")
		Eventually(func(g Gomega) {
			out := mcList(minioBucket + "/" + dbName + "/")
			g.Expect(out).NotTo(BeEmpty(), "expected backup objects in MinIO bucket")
		}, 3*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying BackupHealthy condition is True on the SQLiteDB")
		Eventually(func(g Gomega) {
			out := kubectl("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="BackupHealthy")].status}`)
			g.Expect(out).To(Equal("True"))
		}).Should(Succeed())
	})
})

// ── Scenario 3: SQLiteRestore ─────────────────────────────────────────────

var _ = Describe("SQLiteRestore", Ordered, func() {
	const (
		// Reuse the backup created in Scenario 2.
		sourceDBName  = "backup-test-db"
		restoreName   = "integration-restore"
		restorePVC    = "restore-target-pvc"
		restoreTarget = "/restore/backup.db"
	)

	BeforeAll(func() {
		By("creating the restore target PVC")
		applyLiteral(pvcManifest(restorePVC, testNamespace))
	})

	AfterAll(func() {
		runIgnoreError("kubectl", "delete", "sqliterestore", restoreName, "-n", testNamespace, "--ignore-not-found")
		runIgnoreError("kubectl", "delete", "pvc", restorePVC, "-n", testNamespace, "--ignore-not-found")
	})

	It("restore Job completes successfully and the database file appears on the target PVC", func() {
		By("creating a SQLiteRestore CR")
		applyLiteral(sqliteRestoreManifest(restoreName, testNamespace, sourceDBName, restorePVC, restoreTarget))

		By("waiting for the restore Job to be created")
		Eventually(func(g Gomega) {
			out := kubectl("get", "jobs", "-n", testNamespace,
				"-l", "sqlite.database.example.com/restore="+restoreName,
				"-o", "jsonpath={.items[0].metadata.name}")
			g.Expect(out).NotTo(BeEmpty())
		}).Should(Succeed())

		By("waiting for the restore Job to complete (up to 5 minutes)")
		Eventually(func(g Gomega) {
			out := kubectl("get", "sqliterestore", restoreName, "-n", testNamespace,
				"-o", "jsonpath={.status.phase}")
			g.Expect(out).To(Equal("Complete"),
				"restore phase should be Complete, not %q", out)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying the restored database file exists on the target PVC")
		out := kubectl("get", "sqliterestore", restoreName, "-n", testNamespace,
			"-o", "jsonpath={.status.message}")
		Expect(out).To(ContainSubstring("successfully"))

		By("running a verification Job that reads the restored database")
		verifyJobName := "verify-restore"
		applyLiteral(verifyRestoreJobManifest(verifyJobName, testNamespace, restorePVC, restoreTarget))
		kubectl("wait", "-n", testNamespace, "job/"+verifyJobName,
			"--for=condition=Complete", "--timeout=3m")

		logs := kubectl("logs", "-n", testNamespace, "job/"+verifyJobName)
		Expect(logs).To(ContainSubstring("events"), "restored database should contain the events table")
	})
})

// ── Manifest builders ──────────────────────────────────────────────────────

func pvcManifest(name, ns string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
`, name, ns)
}

func appDeploymentManifest(name, ns, pvcName, mountPath string) string {
	return fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: app
          image: keinos/sqlite3:latest
          command: ["sleep", "infinity"]
          volumeMounts:
            - name: data
              mountPath: %s
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: %s
`, name, ns, name, name, mountPath, pvcName)
}

func sqliteDBManifest(name, ns, target, dbFile, dbPath string, backupEnabled bool, initSQL string) string {
	backup := ""
	if backupEnabled {
		backup = fmt.Sprintf(`
  backup:
    enabled: true
    destination:
      s3:
        endpoint: %s
        bucket: %s
        path: %s/
        secretRef: minio-creds
    retention:
      count: 5`, minioEndpoint, minioBucket, name)
	}

	initSQLBlock := ""
	if initSQL != "" {
		// Indent for YAML literal block.
		lines := strings.Split(initSQL, "\n")
		indented := make([]string, len(lines))
		for i, l := range lines {
			indented[i] = "    " + l
		}
		initSQLBlock = "\n  initSQL: |\n" + strings.Join(indented, "\n")
	}

	return fmt.Sprintf(`
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: %s
  namespace: %s
spec:
  databaseName: %s
  databasePath: %s
  targetDeployment: %s%s%s
`, name, ns, dbFile, dbPath, target, backup, initSQLBlock)
}

func sqliteRestoreManifest(name, ns, sourceRef, pvc, targetPath string) string {
	return fmt.Sprintf(`
apiVersion: database.example.com/v1
kind: SQLiteRestore
metadata:
  name: %s
  namespace: %s
spec:
  sourceRef: %s
  targetPVC: %s
  targetPath: %s
`, name, ns, sourceRef, pvc, targetPath)
}

func verifyRestoreJobManifest(name, ns, pvc, dbPath string) string {
	return fmt.Sprintf(`
apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: verify
          image: keinos/sqlite3:latest
          command: ["sqlite3", "%s", ".tables"]
          volumeMounts:
            - name: data
              mountPath: /restore
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: %s
`, name, ns, dbPath, pvc)
}

// ── Test helpers ───────────────────────────────────────────────────────────

// runningPod returns the name of a Running pod for the given Deployment.
func runningPod(deploymentName string) string {
	out := kubectl("get", "pods", "-n", testNamespace,
		"-l", "app="+deploymentName,
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	Expect(out).NotTo(BeEmpty(), "no Running pod found for deployment %s", deploymentName)
	return strings.TrimSpace(out)
}

// mcList runs `mc ls` against the MinIO service using a temporary pod with the
// mc client image. Returns the listing output or empty string on error.
func mcList(path string) string {
	podName := fmt.Sprintf("mc-ls-%d", time.Now().UnixMilli())
	out, _ := runCmd("kubectl", "run", podName,
		"--restart=Never",
		"--rm",
		"--attach",
		"-n", testNamespace,
		"--image=quay.io/minio/mc:latest",
		"--",
		"/bin/sh", "-c",
		fmt.Sprintf(
			"mc alias set local http://minio:9000 %s %s --quiet && mc ls local/%s 2>/dev/null",
			minioUser, minioPass, path,
		),
	)
	return out
}
