# SQLite Operator

[![Tests](https://github.com/jlaska/sqlite-operator/actions/workflows/test.yml/badge.svg)](https://github.com/jlaska/sqlite-operator/actions/workflows/test.yml)
[![CI/CD Pipeline](https://github.com/jlaska/sqlite-operator/actions/workflows/ci.yaml/badge.svg)](https://github.com/jlaska/sqlite-operator/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/jlaska/sqlite-operator)](https://goreportcard.com/report/github.com/jlaska/sqlite-operator)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Container Image](https://img.shields.io/badge/container-quay.io-red)](https://quay.io/repository/jlaska/sqlite-operator)
[![Kubernetes](https://img.shields.io/badge/kubernetes-v1.11.3+-blue.svg)](https://kubernetes.io/)
[![Go Version](https://img.shields.io/badge/go-v1.24+-blue.svg)](https://golang.org/)

A cloud-native Kubernetes operator for managing SQLite databases with persistent storage, initialization, and backup capabilities.

## Features

- **Declarative Management**: Define SQLite databases using Kubernetes CRDs
- **Persistent Storage**: Automatic PVC creation with configurable storage classes
- **Database Initialization**: Execute SQL scripts during database creation  
- **Backup Support**: Configurable backup schedules and retention
- **Scaling**: Support for read replicas
- **Cloud Native**: Follows Kubernetes best practices and patterns

## Quick Start

```bash
# Deploy the operator
kubectl apply -k https://github.com/jlaska/sqlite-operator/deploy

# Create a SQLite database
kubectl apply -f - <<EOF
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: my-database
spec:
  databaseName: "app_db"
  storage:
    size: "2Gi"
    storageClass: "longhorn-unreplicated-besteffort"
  initSQL: |
    CREATE TABLE users (
      id INTEGER PRIMARY KEY,
      name TEXT NOT NULL,
      email TEXT UNIQUE
    );
    INSERT INTO users (name, email) VALUES ('Alice', 'alice@example.com');
EOF
```

## Documentation

- **📦 [Installation Guide](./docs/INSTALL.md)** - Deploy the operator to your cluster
- **📖 [Usage Guide](./docs/USAGE.md)** - Create databases and connect applications  
- **🔧 [Build Guide](./docs/BUILD.md)** - Development and contribution instructions

## Support

- 🐛 [Report Issues](https://github.com/jlaska/sqlite-operator/issues)
- 💬 [Discussions](https://github.com/jlaska/sqlite-operator/discussions)

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](./LICENSE) for details.
