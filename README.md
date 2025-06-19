# SQLite Operator

[![CI/CD Pipeline](https://github.com/jlaska/sqlite-operator/actions/workflows/ci.yaml/badge.svg)](https://github.com/jlaska/sqlite-operator/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/jlaska/sqlite-operator)](https://goreportcard.com/report/github.com/jlaska/sqlite-operator)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Container Image](https://img.shields.io/badge/container-quay.io-red)](https://quay.io/repository/jlaska/sqlite-operator)
[![Kubernetes](https://img.shields.io/badge/kubernetes-v1.11.3+-blue.svg)](https://kubernetes.io/)
[![Go Version](https://img.shields.io/badge/go-v1.24+-blue.svg)](https://golang.org/)

A cloud-native Kubernetes operator for managing SQLite databases with persistent storage, initialization, and backup capabilities.

## Description

The SQLite Operator enables you to deploy and manage SQLite databases as first-class Kubernetes resources. It provides:

- **Declarative Management**: Define SQLite databases using Kubernetes CRDs
- **Persistent Storage**: Automatic PVC creation and management
- **Database Initialization**: Execute SQL scripts during database creation  
- **Backup Support**: Configurable backup schedules and retention
- **Scaling**: Support for read replicas
- **Cloud Native**: Follows Kubernetes best practices and patterns

## Quick Start

### Deploy the Operator

```bash
# Deploy operator and CRD
kubectl apply -k https://github.com/jlaska/sqlite-operator//deploy

# Create a sample SQLite database
kubectl apply -k https://github.com/jlaska/sqlite-operator//deploy/samples
```

### Create Your First Database

```yaml
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: my-database
spec:
  databaseName: "app_db"
  storageSize: "1Gi"
  initSQL: |
    CREATE TABLE users (
      id INTEGER PRIMARY KEY,
      name TEXT NOT NULL,
      email TEXT UNIQUE
    );
    INSERT INTO users (name, email) VALUES ('Alice', 'alice@example.com');
```

## Installation

### Prerequisites
- Kubernetes v1.11.3+ cluster
- kubectl configured for your cluster

### Installation Options

#### Option 1: Quick Install (Recommended)
```bash
kubectl apply -k https://github.com/jlaska/sqlite-operator//deploy
```

#### Option 2: From Release
```bash
# Get latest release manifests
kubectl apply -f https://github.com/jlaska/sqlite-operator/releases/latest/download/sqlite-operator.yaml
```

#### Option 3: Development Build
```bash
# Clone and build
git clone https://github.com/jlaska/sqlite-operator.git
cd sqlite-operator
make deploy IMG=quay.io/jlaska/sqlite-operator:latest
```

### Verify Installation
```bash
# Check operator is running
kubectl get pods -n sqlite-operator-system

# Create a test database
kubectl apply -k deploy/samples/

# Check database status
kubectl get sqlitedb
```

## Usage

### SQLiteDB Spec Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `databaseName` | string | Name of the SQLite database file | Yes |
| `storageSize` | string | Size of persistent storage (default: 1Gi) | No |
| `replicas` | int32 | Number of replicas (default: 1) | No |
| `initSQL` | string | SQL statements to run during initialization | No |
| `backupEnabled` | bool | Enable automatic backups | No |
| `backupSchedule` | string | Backup schedule in cron format | No |

### Examples

#### Basic Database
```yaml
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: simple-db
spec:
  databaseName: "simple"
  storageSize: "500Mi"
```

#### Database with Initialization
```yaml
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: app-database
spec:
  databaseName: "myapp"
  storageSize: "2Gi"
  initSQL: |
    CREATE TABLE products (
      id INTEGER PRIMARY KEY,
      name TEXT NOT NULL,
      price REAL,
      created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );
    INSERT INTO products (name, price) VALUES 
      ('Widget A', 9.99),
      ('Widget B', 19.99);
```

#### Database with Backups
```yaml
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: production-db
spec:
  databaseName: "prod"
  storageSize: "10Gi"
  replicas: 3
  backupEnabled: true
  backupSchedule: "0 2 * * *"  # Daily at 2 AM
```

## Development

### Prerequisites
- Go v1.24.0+
- Docker v17.03+
- kubebuilder v3.0+

### Build and Test
```bash
# Clone the repository
git clone https://github.com/jlaska/sqlite-operator.git
cd sqlite-operator

# Run tests
make test

# Build operator image
make docker-build IMG=myregistry/sqlite-operator:dev

# Deploy to cluster
make deploy IMG=myregistry/sqlite-operator:dev
```

### Local Development
```bash
# Install CRDs
make install

# Run operator locally
make run
```

## Uninstall

### Remove SQLite Databases
```bash
# Delete all SQLiteDB instances
kubectl delete sqlitedb --all

# Or delete specific instances
kubectl delete -k deploy/samples/
```

### Remove Operator
```bash
# Remove operator deployment
kubectl delete -k https://github.com/jlaska/sqlite-operator//deploy

# Or using make (for development)
make undeploy
make uninstall
```

## Architecture

The SQLite Operator consists of:

- **Custom Resource Definition (CRD)**: Defines the SQLiteDB resource schema
- **Controller**: Watches SQLiteDB resources and manages their lifecycle
- **Reconciler**: Ensures desired state matches actual state by creating:
  - Kubernetes Deployments for SQLite containers
  - PersistentVolumeClaims for data storage
  - Services for database access
  - ConfigMaps for initialization scripts

## Monitoring

The operator exposes Prometheus metrics on port 8443:

```bash
# Port-forward to access metrics
kubectl port-forward -n sqlite-operator-system svc/sqlite-operator-controller-manager-metrics-service 8443:8443

# View metrics
curl -k https://localhost:8443/metrics
```

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

### Development Workflow
- All changes must include tests
- Code must pass linting (`make lint`)
- PRs require passing CI checks
- Follow [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)

## License

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

## Support

- 📖 [Documentation](./deploy/README.md)
- 🐛 [Issues](https://github.com/jlaska/sqlite-operator/issues)
- 💬 [Discussions](https://github.com/jlaska/sqlite-operator/discussions)
- 🔧 [Build Pipeline](./BUILD.md)