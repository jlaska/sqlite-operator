# Litestream Operator Deployment

This directory contains Kubernetes manifests for deploying the Litestream Operator using Kustomize.

## Directory Structure

```
deploy/
├── kustomization.yaml              # Main kustomization file
├── namespace.yaml                  # Operator namespace
├── serviceaccount.yaml            # Service account for operator
├── clusterrole.yaml               # RBAC permissions
├── clusterrolebinding.yaml        # Bind permissions to service account
├── deployment.yaml                # Operator deployment
├── service.yaml                   # Metrics service
├── controller_manager_config.yaml # Controller configuration
├── litestream.io_litestreamreplicas.yaml # LitestreamReplica CRD
└── samples/
    ├── kustomization.yaml         # Sample resources kustomization
    └── sample-litestreamreplica.yaml       # Example LitestreamReplica resource
```

## Quick Deployment

### 1. Deploy the Operator

```bash
# Deploy the operator and CRD
kubectl apply -k deploy/

# Verify the operator is running
kubectl get pods -n litestream-operator-system
```

### 2. Create a SQLite Database

```bash
# Deploy a sample SQLite database
kubectl apply -k deploy/samples/

# Check the status
kubectl get litestreamreplica
kubectl describe litestreamreplica example-sqlite
```

### 3. Verify the Database is Running

```bash
# Check the created resources
kubectl get pods,svc,pvc -l app=example-sqlite

# Connect to the database (optional)
kubectl exec -it deployment/example-sqlite -- sqlite3 /data/myapp.db ".schema"
```

## Customization

### Building Custom Images

```bash
# Build the operator image
make docker-build IMG=your-registry/litestream-operator:v1.0.0

# Push to registry
make docker-push IMG=your-registry/litestream-operator:v1.0.0
```

### Using Custom Images

Edit `deploy/kustomization.yaml` to use your custom image:

```yaml
images:
- name: controller
  newName: your-registry/litestream-operator
  newTag: v1.0.0
```

### Creating Custom LitestreamReplica Resources

Example LitestreamReplica resource:

```yaml
apiVersion: litestream.io/v1
kind: LitestreamReplica
metadata:
  name: my-database
  namespace: my-namespace
spec:
  databaseName: "app_db"
  storage:
    size: "5Gi"
  replicas: 1
  initSQL: |
    CREATE TABLE products (
      id INTEGER PRIMARY KEY,
      name TEXT NOT NULL,
      price REAL
    );
    INSERT INTO products (name, price) VALUES ('Widget', 9.99);
  backupEnabled: true
  backupSchedule: "0 2 * * *"
```

## LitestreamReplica Spec Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `databaseName` | string | Name of the SQLite database file | Yes |
| `storage.size` | string | Size of persistent storage (default: 1Gi) | No |
| `storage.storageClass` | string | Storage class name for the database | No |
| `replicas` | int32 | Number of replicas (default: 1) | No |
| `initSQL` | string | SQL statements to run during initialization | No |
| `backupEnabled` | bool | Enable automatic backups | No |
| `backupSchedule` | string | Backup schedule in cron format | No |

## LitestreamReplica Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | Current phase (Creating, Pending, Ready) |
| `ready` | bool | Whether the database is ready |
| `databaseSize` | string | Current database file size |
| `lastBackup` | metav1.Time | Timestamp of last backup |
| `podName` | string | Name of the pod running the database |
| `conditions` | []metav1.Condition | Detailed status conditions |

## Monitoring

The operator exposes metrics on port 8443. You can scrape these metrics using Prometheus:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: litestream-operator-metrics
  namespace: litestream-operator-system
spec:
  ports:
  - name: https
    port: 8443
    targetPort: https
  selector:
    control-plane: controller-manager
```

## Troubleshooting

### Check Operator Logs

```bash
kubectl logs -n litestream-operator-system deployment/litestream-operator-controller-manager -c manager
```

### Check LitestreamReplica Events

```bash
kubectl describe litestreamreplica <name>
```

### Check Created Resources

```bash
kubectl get all -l app=<litestreamreplica-name>
```

## ArgoCD Integration

This operator works well with ArgoCD. Create an Application pointing to the `deploy/` directory:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: litestream-operator
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://your-repo/litestream-operator.git
    targetRevision: HEAD
    path: deploy
  destination:
    server: https://kubernetes.default.svc
    namespace: litestream-operator-system
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
```