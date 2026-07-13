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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigsyaml "sigs.k8s.io/yaml"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
)

// All integration scenarios run in a single Ordered container so Ginkgo's
// randomiser cannot reorder them. Scenario 3 (Restore) depends on the backup
// data created by Scenario 2 (Backup), which in turn builds on the injection
// verified in Scenario 1.
var _ = Describe("Integration", Ordered, func() {

	// ── Scenario 1: Sidecar Injection ─────────────────────────────────────

	Describe("Sidecar Injection", func() {
		const (
			appName = "inject-test-app"
			dbName  = "inject-test-db"
			pvcName = "inject-test-pvc"
			dbFile  = "inject.db"
			dbPath  = "/data"
		)

		BeforeAll(func() {
			By("creating test PVC")
			applyLiteral(pvcManifest(pvcName, testNamespace))

			By("creating test app Deployment")
			applyLiteral(appDeploymentManifest(appName, testNamespace, pvcName, dbPath))

			By("waiting for initial app pod to be Running")
			kubectl("wait", "-n", testNamespace, "deployment/"+appName,
				"--for=condition=Available", "--timeout=3m")
		})

		AfterAll(func() {
			runIgnoreError("kubectl", "delete", "sqlitedb", dbName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
			runIgnoreError("kubectl", "delete", "deployment", appName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
			runIgnoreError("kubectl", "delete", "pvc", pvcName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		})

		It("injects the Litestream sidecar into new pods after SQLiteDB CR is created", func() {
			By("creating a SQLiteDB CR (backup disabled — just test injection)")
			applyLiteral(sqliteDBManifest(dbName, testNamespace, appName, dbFile, dbPath, false, ""))

			By("waiting for the controller to label and annotate the pod template")
			Eventually(func(g Gomega) {
				out, err := kubectlQ("get", "deployment", appName, "-n", testNamespace,
					"-o", `jsonpath={.spec.template.metadata.labels.sqlite\.database\.example\.com/inject}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("true"))
			}).Should(Succeed())

			By("waiting for rolling update to produce a pod with the Litestream sidecar")
			Eventually(func(g Gomega) {
				out, err := kubectlQ("get", "pods", "-n", testNamespace,
					"-l", "app="+appName,
					"-o", `jsonpath={range .items[*]}{range .spec.containers[*]}{.name}{"\n"}{end}{end}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("litestream"))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("confirming SidecarInjected condition is True on the SQLiteDB")
			Eventually(func(g Gomega) {
				out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
					"-o", `jsonpath={.status.conditions[?(@.type=="SidecarInjected")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("True"))
			}).Should(Succeed())
		})
	})

	// ── Scenario 2: Backup to MinIO ───────────────────────────────────────

	Describe("Backup to MinIO", func() {
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
			DeferCleanup(func() { dumpReplicationDiagnostics(appName, dbName, dbFile) })

			By("creating test PVC")
			applyLiteral(pvcManifest(pvcName, testNamespace))

			By("creating test app Deployment")
			applyLiteral(appDeploymentManifest(appName, testNamespace, pvcName, dbPath))

			By("waiting for initial pod to be Running")
			kubectl("wait", "-n", testNamespace, "deployment/"+appName,
				"--for=condition=Available", "--timeout=3m")

			By("creating SQLiteDB CR with backup enabled and initSQL")
			applyLiteral(sqliteDBManifest(dbName, testNamespace, appName, dbFile, dbPath, true, initSQL))

			By("waiting for sidecar injection rollout to complete (2/2 containers)")
			Eventually(func(g Gomega) {
				out, err := kubectlQ("get", "pods", "-n", testNamespace,
					"-l", "app="+appName,
					"-o", `jsonpath={range .items[*]}{range .spec.containers[*]}{.name}{"\n"}{end}{end}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("litestream"))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			// Leave the SQLiteDB and its backup intact — Scenario 3 restores from it.
			runIgnoreError("kubectl", "delete", "deployment", appName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		})

		It("init container applies initSQL and creates the events table", func() {
			podName := runningPod(appName)
			Eventually(func(g Gomega) {
				out, err := kubectlQ("exec", "-n", testNamespace, podName, "-c", "app",
					"--", "sqlite3", dbPath+"/"+dbFile, ".tables")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("events"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("Litestream replicates WAL changes to the MinIO bucket", func() {
			podName := runningPod(appName)

			By("writing a row to the database")
			kubectl("exec", "-n", testNamespace, podName, "-c", "app",
				"--", "sqlite3", dbPath+"/"+dbFile,
				"INSERT INTO events(message) VALUES('integration-test-backup');")

			By("waiting for Litestream to replicate to MinIO")
			Eventually(func(g Gomega) {
				// Check the expected path first.
				out := mcList(minioBucket + "/" + dbName + "/")
				if out == "" {
					// Fall back: check the whole bucket so a path mismatch surfaces immediately.
					all, _ := kubectlQ("exec", "-n", testNamespace, "mc-client", "--",
						"/bin/sh", "-c", "mc ls --recursive local/"+minioBucket+"/")
					g.Expect(out).NotTo(BeEmpty(),
						"expected backup objects at %s/%s/ — full bucket contents:\n%s",
						minioBucket, dbName, all)
				} else {
					g.Expect(out).NotTo(BeEmpty(), "expected backup objects in MinIO bucket")
				}
			}, 3*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying BackupHealthy condition is True on the SQLiteDB")
			Eventually(func(g Gomega) {
				out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
					"-o", `jsonpath={.status.conditions[?(@.type=="BackupHealthy")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("True"))
			}).Should(Succeed())
		})
	})

	// ── Scenario 3: SQLiteRestore ─────────────────────────────────────────

	Describe("SQLiteRestore", func() {
		const (
			sourceDBName  = "backup-test-db" // backup created by Scenario 2
			restoreName   = "integration-restore"
			restorePVC    = "restore-target-pvc"
			restoreTarget = "/restore/backup.db"
		)

		BeforeAll(func() {
			By("creating the restore target PVC")
			applyLiteral(pvcManifest(restorePVC, testNamespace))
		})

		AfterAll(func() {
			// --wait=false: completed restore job pods hold PVC references; waiting for
			// the PVC to fully disappear blocks indefinitely until async GC removes them.
			// AfterSuite namespace deletion cleans up any remaining resources.
			runIgnoreError("kubectl", "delete", "sqliterestore", restoreName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
			runIgnoreError("kubectl", "delete", "sqlitedb", sourceDBName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
			runIgnoreError("kubectl", "delete", "pvc", restorePVC, "-n", testNamespace, "--ignore-not-found", "--wait=false")
			runIgnoreError("kubectl", "delete", "pvc", "backup-test-pvc", "-n", testNamespace, "--ignore-not-found", "--wait=false")
		})

		It("restore Job completes and database file appears on the target PVC", func() {
			By("creating a SQLiteRestore CR")
			applyLiteral(sqliteRestoreManifest(restoreName, testNamespace, sourceDBName, restorePVC, restoreTarget))

			By("waiting for the restore Job to be created")
			Eventually(func(g Gomega) {
				out, err := kubectlQ("get", "jobs", "-n", testNamespace,
					"-l", "sqlite.database.example.com/restore="+restoreName,
					"-o", "jsonpath={.items[0].metadata.name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
			}).Should(Succeed())

			By("waiting for the restore Job to Complete (up to 5 minutes)")
			Eventually(func(g Gomega) {
				phase, err := kubectlQ("get", "sqliterestore", restoreName, "-n", testNamespace,
					"-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				if phase == "Failed" || phase == "Running" {
					jobName, _ := kubectlQ("get", "sqliterestore", restoreName, "-n", testNamespace,
						"-o", "jsonpath={.status.jobName}")
					if jobName != "" {
						// Show pod status — useful when kubectl logs times out (pod pending/between retries).
						pods, _ := kubectlQ("get", "pods", "-n", testNamespace,
							"-l", "sqlite.database.example.com/restore="+restoreName, "-o", "wide")
						GinkgoWriter.Printf("\n=== restore Job pods ===\n%s\n", pods)
						// --previous gets logs from the last terminated container
						// even when the pod is currently between retries.
						// --request-timeout prevents blocking indefinitely if the pod
						// is in a transient state (Pending, initializing).
						logs, _ := kubectlQ("logs", "-n", testNamespace, "job/"+jobName, "--previous", "--tail=50", "--request-timeout=15s")
						if logs == "" {
							logs, _ = kubectlQ("logs", "-n", testNamespace, "job/"+jobName, "--tail=50", "--request-timeout=15s")
						}
						GinkgoWriter.Printf("=== restore Job logs ===\n%s\n========================\n", logs)
					}
				}
				g.Expect(phase).To(Equal("Complete"))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("running a verification Job that reads the restored database")
			verifyJobName := "verify-restore"
			applyLiteral(verifyRestoreJobManifest(verifyJobName, testNamespace, restorePVC, restoreTarget))
			kubectl("wait", "-n", testNamespace, "job/"+verifyJobName,
				"--for=condition=Complete", "--timeout=3m")

			logs := kubectl("logs", "-n", testNamespace, "job/"+verifyJobName)
			Expect(logs).To(ContainSubstring("events"),
				"restored database should contain the events table")
		})
	})

}) // end Integration Ordered

// ── Scenario: Manual Replication Pause ───────────────────────────────────────

var _ = Describe("Replication Pause", Ordered, func() {
	const (
		appName = "pause-test-app"
		dbName  = "pause-test-db"
		pvcName = "pause-test-pvc"
		dbFile  = "pause.db"
		dbPath  = "/data"
	)

	BeforeAll(func() {
		By("creating PVC, Deployment, and SQLiteDB CR with backup enabled")
		applyLiteral(pvcManifest(pvcName, testNamespace))
		applyLiteral(appDeploymentManifest(appName, testNamespace, pvcName, dbPath))
		kubectl("wait", "-n", testNamespace, "deployment/"+appName,
			"--for=condition=Available", "--timeout=3m")
		applyLiteral(sqliteDBManifest(dbName, testNamespace, appName, dbFile, dbPath, true, ""))

		By("waiting for sidecar injection and BackupHealthy=True")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="BackupHealthy")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		// Ensure pause annotation is removed even if test fails mid-way.
		runIgnoreError("kubectl", "annotate", "sqlitedb", dbName, "-n", testNamespace,
			"sqlite.database.example.com/pause-", "--ignore-not-found")
		runIgnoreError("kubectl", "delete", "sqlitedb", dbName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		runIgnoreError("kubectl", "delete", "deployment", appName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		runIgnoreError("kubectl", "delete", "pvc", pvcName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
	})

	It("pauses replication when annotation is set and resumes when removed", func() {
		By("setting pause annotation on SQLiteDB")
		kubectl("annotate", "sqlitedb", dbName, "-n", testNamespace,
			"sqlite.database.example.com/pause=true", "--overwrite")

		By("waiting for ConfigMap to reflect pause (dbs: [])")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "configmap", dbName+"-litestream", "-n", testNamespace,
				"-o", `jsonpath={.data.litestream\.yml}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("dbs: []\n"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying ReplicationPaused condition is True")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="ReplicationPaused")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}).Should(Succeed())

		By("verifying phase is Paused")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Paused"))
		}).Should(Succeed())

		By("removing pause annotation")
		kubectl("annotate", "sqlitedb", dbName, "-n", testNamespace,
			"sqlite.database.example.com/pause-")

		By("waiting for ConfigMap to restore normal config")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "configmap", dbName+"-litestream", "-n", testNamespace,
				"-o", `jsonpath={.data.litestream\.yml}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("dbs:"))
			g.Expect(out).NotTo(Equal("dbs: []\n"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying phase returns to Ready")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Ready"))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})
})

// ── Scenario: Multi-replica rejection ─────────────────────────────────────────
//
// Verifies the full end-to-end webhook rejection when a user targets a Deployment
// that has replicas > 1. Litestream requires exactly one writer; the admitting
// webhook must reject the SQLiteDB create at the API layer.

var _ = Describe("Multi-Replica Rejection", func() {
	const (
		appName = "multi-rep-app"
		dbName  = "multi-rep-db"
		pvcName = "multi-rep-pvc"
	)

	BeforeEach(func() {
		By("creating PVC and a 2-replica Deployment")
		applyLiteral(pvcManifest(pvcName, testNamespace))
		applyLiteral(fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 2
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
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: %s
`, appName, testNamespace, appName, appName, pvcName))
	})

	AfterEach(func() {
		runIgnoreError("kubectl", "delete", "sqlitedb", dbName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		runIgnoreError("kubectl", "delete", "deployment", appName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		runIgnoreError("kubectl", "delete", "pvc", pvcName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
	})

	It("webhook rejects SQLiteDB pointing at a multi-replica Deployment", func() {
		By("attempting to create a SQLiteDB targeting the 2-replica Deployment")
		manifest := fmt.Sprintf(`
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: %s
  namespace: %s
spec:
  databaseName: app.db
  databasePath: /data
  targetDeployment: %s
`, dbName, testNamespace, appName)

		out, err := applyLiteralQ(manifest)

		// The webhook must reject the create with a validation error.
		By("expecting admission rejection (replicas > 1 is a hard error)")
		Expect(err).To(HaveOccurred(), "webhook should reject SQLiteDB targeting a 2-replica Deployment")
		Expect(strings.ToLower(out)).To(ContainSubstring("replicas"),
			"rejection message should mention replicas; got: %s", out)
	})
})

// ── Scenario: Archive Check — Data Loss Recovery ──────────────────────────────

var _ = Describe("Archive Check — Data Loss Recovery", Ordered, func() {
	const (
		appName   = "archive-check-app"
		dbName    = "archive-check-db"
		pvcName   = "archive-check-pvc"
		dbFile    = "archive.db"
		dbPath    = "/data"
		initSQL   = "CREATE TABLE IF NOT EXISTS items (id INTEGER PRIMARY KEY, name TEXT);"
		itemValue = "archive-check-test-item"
	)

	BeforeAll(func() {
		DeferCleanup(func() { dumpReplicationDiagnostics(appName, dbName, dbFile) })

		By("creating PVC, Deployment, and SQLiteDB CR with backup enabled and initSQL")
		applyLiteral(pvcManifest(pvcName, testNamespace))
		applyLiteral(appDeploymentManifest(appName, testNamespace, pvcName, dbPath))
		kubectl("wait", "-n", testNamespace, "deployment/"+appName,
			"--for=condition=Available", "--timeout=3m")
		applyLiteral(sqliteDBManifest(dbName, testNamespace, appName, dbFile, dbPath, true, initSQL))

		By("waiting for sidecar injection and BackupHealthy=True")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="BackupHealthy")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("writing test data to the database")
		podName := runningPod(appName)
		kubectl("exec", "-n", testNamespace, podName, "-c", "app",
			"--", "sqlite3", dbPath+"/"+dbFile,
			"INSERT INTO items(name) VALUES('"+itemValue+"');")

		By("waiting for Litestream to replicate the row to MinIO")
		Eventually(func(g Gomega) {
			out := mcList(minioBucket + "/" + dbName + "/")
			if out == "" {
				all, _ := kubectlQ("exec", "-n", testNamespace, "mc-client", "--",
					"/bin/sh", "-c", "mc ls --recursive local/"+minioBucket+"/")
				g.Expect(out).NotTo(BeEmpty(),
					"expected backup objects at %s/%s/ — full bucket contents:\n%s",
					minioBucket, dbName, all)
			} else {
				g.Expect(out).NotTo(BeEmpty(), "expected backup objects in MinIO bucket")
			}
		}, 3*time.Minute, 10*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		runIgnoreError("kubectl", "delete", "sqliterestore", "archive-check-restore", "-n", testNamespace, "--ignore-not-found", "--wait=false")
		runIgnoreError("kubectl", "delete", "sqlitedb", dbName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		runIgnoreError("kubectl", "delete", "deployment", appName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
		runIgnoreError("kubectl", "delete", "pvc", pvcName, "-n", testNamespace, "--ignore-not-found", "--wait=false")
	})

	It("archive-check blocks pod startup when DB is missing and S3 has data", func() {
		podName := runningPod(appName)

		By("deleting the database file from the PVC (simulating data loss)")
		kubectl("exec", "-n", testNamespace, podName, "-c", "app",
			"--", "rm", "-f", dbPath+"/"+dbFile)

		By("scaling Deployment to 0 and back to 1 to force pod restart")
		kubectl("scale", "deployment", appName, "-n", testNamespace, "--replicas=0")
		// kubectl rollout status correctly handles scale-to-0 completion.
		// --for=jsonpath={.status.replicas}=0 is unreliable: Kubernetes omits
		// the field when it reaches zero, so the jsonpath never matches.
		kubectl("rollout", "status", "deployment/"+appName, "-n", testNamespace, "--timeout=2m")
		kubectl("scale", "deployment", appName, "-n", testNamespace, "--replicas=1")

		By("waiting for archive-check init container to fail (pod blocked)")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "pods", "-n", testNamespace,
				"-l", "app="+appName,
				"-o", `jsonpath={range .items[*]}{range .status.initContainerStatuses[*]}{.name}={.state.terminated.exitCode}{"\n"}{end}{end}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("litestream-archive-check=1"),
				"expected archive-check init container to exit 1")
		}, 3*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying ArchiveCheckFailed condition on SQLiteDB")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="ArchiveCheckFailed")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}).Should(Succeed())
	})

	It("SQLiteRestore recovers data and allows pod to restart successfully", func() {
		By("creating a SQLiteRestore CR targeting the same PVC")
		applyLiteral(sqliteRestoreManifest("archive-check-restore", testNamespace, dbName, pvcName, dbPath+"/"+dbFile))

		By("waiting for restore to reach Complete phase")
		Eventually(func(g Gomega) {
			phase, err := kubectlQ("get", "sqliterestore", "archive-check-restore", "-n", testNamespace,
				"-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			if phase == "Failed" {
				jobName, _ := kubectlQ("get", "sqliterestore", "archive-check-restore", "-n", testNamespace,
					"-o", "jsonpath={.status.jobName}")
				if jobName != "" {
					logs, _ := kubectlQ("logs", "-n", testNamespace, "job/"+jobName, "--tail=50", "--request-timeout=15s")
					GinkgoWriter.Printf("\n=== restore Job logs ===\n%s\n========================\n", logs)
				}
			}
			g.Expect(phase).To(Equal("Complete"))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("waiting for the pod to restart successfully after restore")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "pods", "-n", testNamespace,
				"-l", "app="+appName,
				"--field-selector=status.phase=Running",
				"-o", "jsonpath={.items[0].metadata.name}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty())
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying restored data is present in the database")
		restoredPod := runningPod(appName)
		Eventually(func(g Gomega) {
			out, err := kubectlQ("exec", "-n", testNamespace, restoredPod, "-c", "app",
				"--", "sqlite3", dbPath+"/"+dbFile,
				"SELECT name FROM items WHERE name='"+itemValue+"';")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring(itemValue))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying BackupHealthy=True after restore (Litestream resumed)")
		Eventually(func(g Gomega) {
			out, err := kubectlQ("get", "sqlitedb", dbName, "-n", testNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="BackupHealthy")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}, 3*time.Minute, 10*time.Second).Should(Succeed())
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
	db := &databasev1.SQLiteDB{
		TypeMeta:   metav1.TypeMeta{APIVersion: "database.example.com/v1", Kind: "SQLiteDB"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: databasev1.SQLiteDBSpec{
			DatabaseName:     dbFile,
			DatabasePath:     dbPath,
			TargetDeployment: target,
			InitSQL:          initSQL,
		},
	}
	if backupEnabled {
		db.Spec.Backup = databasev1.BackupSpec{
			Enabled: true,
			Destination: databasev1.BackupDestination{
				S3: &databasev1.S3Destination{
					Endpoint:  minioEndpoint,
					Bucket:    minioBucket,
					Path:      name + "/",
					SecretRef: "minio-creds",
				},
			},
			Retention: databasev1.RetentionPolicy{Duration: "720h"},
		}
	}
	data, err := sigsyaml.Marshal(db)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	GinkgoWriter.Printf("--- SQLiteDB CR YAML applied (%s) ---\n%s\n", name, string(data))
	return string(data)
}

func sqliteRestoreManifest(name, ns, sourceRef, pvc, targetPath string) string {
	restore := &databasev1.SQLiteRestore{
		TypeMeta:   metav1.TypeMeta{APIVersion: "database.example.com/v1", Kind: "SQLiteRestore"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: databasev1.SQLiteRestoreSpec{
			SourceRef:  sourceRef,
			TargetPVC:  pvc,
			TargetPath: targetPath,
		},
	}
	data, err := sigsyaml.Marshal(restore)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return string(data)
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
      # Run as root so we can read the litestream-restored file (created as root:root 600).
      securityContext:
        runAsUser: 0
        runAsGroup: 0
      containers:
        - name: verify
          image: keinos/sqlite3:latest
          command: ["sqlite3", "%s", ".tables"]
          securityContext:
            runAsUser: 0
            allowPrivilegeEscalation: false
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
	var podName string
	Eventually(func(g Gomega) {
		out, err := kubectlQ("get", "pods", "-n", testNamespace,
			"-l", "app="+deploymentName,
			"--field-selector=status.phase=Running",
			"-o", "jsonpath={.items[0].metadata.name}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).NotTo(BeEmpty())
		podName = strings.TrimSpace(out)
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
	return podName
}

// mcList runs `mc ls` against the MinIO service using the persistent mc-client pod.
// Returns the listing output on success, empty string on any error.
// The mc alias is pre-configured in BeforeSuite so no setup is needed here.
func mcList(path string) string {
	out, err := kubectlQ("exec", "-n", testNamespace, "mc-client", "--",
		"/bin/sh", "-c",
		fmt.Sprintf("mc ls local/%s", path),
	)
	if err != nil {
		return ""
	}
	return out
}

// dumpReplicationDiagnostics prints a diagnostic snapshot to help debug Litestream
// replication failures. Call via DeferCleanup before polling mc ls so the output
// appears after any failure, regardless of which check timed out.
// dbName is the SQLiteDB CR name (used for ConfigMap lookup: <dbName>-litestream).
// dbFile is the database filename (used for sqlite3 access: /data/<dbFile>).
func dumpReplicationDiagnostics(appName, dbName, dbFile string) {
	GinkgoWriter.Printf("\n====== replication diagnostics: %s / %s ======\n", appName, dbName)

	// 1. Litestream sidecar logs.
	podName, podErr := kubectlQ("get", "pods", "-n", testNamespace,
		"-l", "app="+appName, "-o", "jsonpath={.items[0].metadata.name}")
	podName = strings.TrimSpace(podName)
	if podErr == nil && podName != "" {
		logs, _ := kubectlQ("logs", "-n", testNamespace, podName, "-c", "litestream", "--tail=100")
		GinkgoWriter.Printf("--- Litestream sidecar logs (pod %s) ---\n%s\n", podName, logs)

		journal, _ := kubectlQ("exec", "-n", testNamespace, podName, "-c", "app",
			"--", "sqlite3", "/data/"+dbFile, "PRAGMA journal_mode;")
		GinkgoWriter.Printf("--- SQLite journal_mode ---\n%s\n", journal)
	} else {
		GinkgoWriter.Printf("--- no running pod found for app=%s ---\n", appName)
	}

	// 2. Litestream ConfigMap content (named after the CR, not the db file).
	cm, _ := kubectlQ("get", "configmap", dbName+"-litestream", "-n", testNamespace, "-o", "yaml")
	GinkgoWriter.Printf("--- ConfigMap %s-litestream ---\n%s\n", dbName, cm)

	// 3. Full bucket listing (recursive, entire bucket).
	allObjects, _ := kubectlQ("exec", "-n", testNamespace, "mc-client", "--",
		"/bin/sh", "-c", "mc ls --recursive local/"+minioBucket+"/")
	GinkgoWriter.Printf("--- mc ls --recursive local/%s/ ---\n%s\n", minioBucket, allObjects)

	// 4. mc alias verification.
	aliasList, _ := kubectlQ("exec", "-n", testNamespace, "mc-client", "--",
		"/bin/sh", "-c", "mc alias list local")
	GinkgoWriter.Printf("--- mc alias list local ---\n%s\n", aliasList)

	// 5. Pod container statuses (detect CrashLoopBackOff etc.).
	podStatus, _ := kubectlQ("get", "pods", "-n", testNamespace, "-l", "app="+appName, "-o", "wide")
	GinkgoWriter.Printf("--- pods for %s ---\n%s\n", appName, podStatus)

	GinkgoWriter.Printf("====== end diagnostics ======\n\n")
}
