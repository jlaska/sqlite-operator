# SQLite Operator Usage Guide

This guide demonstrates how to use the SQLite Operator to create SQLite databases and connect applications to them.

## Table of Contents

- [Creating SQLite Databases](#creating-sqlite-databases)
- [Connecting Applications](#connecting-applications)
- [Storage Configuration](#storage-configuration)
- [Database Initialization](#database-initialization)
- [Examples](#examples)
- [Troubleshooting](#troubleshooting)

## Creating SQLite Databases

### Basic SQLite Database

Create a simple SQLite database with default settings:

```yaml
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: my-app-db
  namespace: default
spec:
  databaseName: "appdata"
```

This creates:
- PersistentVolumeClaim: `my-app-db-storage` (1Gi, default storage class)
- Database file: `/data/appdata.db` (inside the SQLite pod)
- Service: `my-app-db-service` for database access

### SQLite Database with Custom Storage

```yaml
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: production-db
  namespace: myapp
spec:
  databaseName: "prod"
  storage:
    size: "10Gi"
    storageClass: "longhorn"
  initSQL: |
    CREATE TABLE users (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      name TEXT NOT NULL,
      email TEXT UNIQUE NOT NULL,
      created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX idx_users_email ON users(email);
```

## Connecting Applications

Applications can connect to SQLite databases by mounting the same PersistentVolumeClaim created by the operator.

### Method 1: Shared Volume Access

Mount the SQLite database storage directly in your application pod:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
        env:
        - name: DATABASE_PATH
          value: "/data/appdata.db"
        volumeMounts:
        - name: sqlite-storage
          mountPath: /data
        command: ["./my-app"]
      volumes:
      - name: sqlite-storage
        persistentVolumeClaim:
          claimName: my-app-db-storage  # Reference the SQLiteDB's PVC
```

**Important Notes:**
- Use `replicas: 1` to avoid SQLite locking issues (SQLite doesn't support concurrent writes)
- The database file path is `/data/{databaseName}.db`
- Mount the PVC with the same name pattern: `{sqlitedb-name}-storage`

### Method 2: Init Container Pattern

Use an init container to ensure the database is ready before starting your application:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app-with-init
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      initContainers:
      - name: wait-for-db
        image: keinos/sqlite3:latest
        command:
        - sh
        - -c
        - |
          until sqlite3 /data/appdata.db "SELECT 1;" > /dev/null 2>&1; do
            echo "Waiting for database to be ready..."
            sleep 2
          done
          echo "Database is ready!"
        volumeMounts:
        - name: sqlite-storage
          mountPath: /data
      containers:
      - name: app
        image: my-app:latest
        env:
        - name: DATABASE_PATH
          value: "/data/appdata.db"
        volumeMounts:
        - name: sqlite-storage
          mountPath: /data
      volumes:
      - name: sqlite-storage
        persistentVolumeClaim:
          claimName: my-app-db-storage
```

### Method 3: Sidecar Pattern

Run your application with a SQLite management sidecar:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app-with-sidecar
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
        env:
        - name: DATABASE_PATH
          value: "/data/appdata.db"
        volumeMounts:
        - name: sqlite-storage
          mountPath: /data
      - name: sqlite-manager
        image: keinos/sqlite3:latest
        command: ["sleep", "infinity"]
        volumeMounts:
        - name: sqlite-storage
          mountPath: /data
        # This sidecar can be used for database maintenance tasks
      volumes:
      - name: sqlite-storage
        persistentVolumeClaim:
          claimName: my-app-db-storage
```

## Storage Configuration

### Storage Classes

Specify custom storage classes for different performance requirements:

```yaml
# High-performance storage
spec:
  storage:
    size: "5Gi"
    storageClass: "ssd-high-iops"

# Cost-effective storage
spec:
  storage:
    size: "50Gi"
    storageClass: "longhorn"

# Default storage (omit storageClass)
spec:
  storage:
    size: "2Gi"
```

### Storage Size Guidelines

| Use Case | Recommended Size | Storage Class |
|----------|------------------|---------------|
| Development/Testing | 1-5Gi | default |
| Small Applications | 5-20Gi | standard |
| Medium Applications | 20-100Gi | ssd |
| Large Applications | 100Gi+ | high-performance |

## Database Initialization

### Basic Table Creation

```yaml
spec:
  databaseName: "blog"
  initSQL: |
    CREATE TABLE posts (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      title TEXT NOT NULL,
      content TEXT,
      created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX idx_posts_created ON posts(created_at);
```

### Complex Initialization with Data

```yaml
spec:
  databaseName: "ecommerce"
  initSQL: |
    -- Create tables
    CREATE TABLE categories (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      name TEXT UNIQUE NOT NULL,
      description TEXT
    );

    CREATE TABLE products (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      name TEXT NOT NULL,
      price DECIMAL(10,2),
      category_id INTEGER,
      stock INTEGER DEFAULT 0,
      FOREIGN KEY (category_id) REFERENCES categories (id)
    );

    -- Insert initial data
    INSERT INTO categories (name, description) VALUES
      ('Electronics', 'Electronic devices and accessories'),
      ('Books', 'Physical and digital books'),
      ('Clothing', 'Apparel and accessories');

    INSERT INTO products (name, price, category_id, stock) VALUES
      ('Laptop', 999.99, 1, 10),
      ('Python Programming', 49.99, 2, 50),
      ('T-Shirt', 19.99, 3, 100);
```

## Examples

### Example 1: Web Application with SQLite

Complete example of a web application using SQLite:

```yaml
# 1. Create the SQLite database
apiVersion: database.example.com/v1
kind: SQLiteDB
metadata:
  name: webapp-db
  namespace: production
spec:
  databaseName: "webapp"
  storage:
    size: "5Gi"
    storageClass: "ssd"
  initSQL: |
    CREATE TABLE users (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      username TEXT UNIQUE NOT NULL,
      email TEXT UNIQUE NOT NULL,
      password_hash TEXT NOT NULL,
      created_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );

    CREATE TABLE sessions (
      id TEXT PRIMARY KEY,
      user_id INTEGER,
      expires_at DATETIME,
      FOREIGN KEY (user_id) REFERENCES users (id)
    );

---
# 2. Deploy the web application
apiVersion: apps/v1
kind: Deployment
metadata:
  name: webapp
  namespace: production
spec:
  replicas: 1
  selector:
    matchLabels:
      app: webapp
  template:
    metadata:
      labels:
        app: webapp
    spec:
      containers:
      - name: webapp
        image: my-webapp:v1.0.0
        ports:
        - containerPort: 8080
        env:
        - name: DATABASE_PATH
          value: "/data/webapp.db"
        - name: PORT
          value: "8080"
        volumeMounts:
        - name: database
          mountPath: /data
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 30
        readinessProbe:
          httpGet:
            path: /ready
            port: 8080
          initialDelaySeconds: 5
      volumes:
      - name: database
        persistentVolumeClaim:
          claimName: webapp-db-storage

---
# 3. Expose the application
apiVersion: v1
kind: Service
metadata:
  name: webapp-service
  namespace: production
spec:
  selector:
    app: webapp
  ports:
  - port: 80
    targetPort: 8080
  type: ClusterIP
```

### Example 2: Microservice with Database Migration

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: db-migration
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
      - name: migrate
        image: migrate/migrate:latest
        command:
        - migrate
        - -path=/migrations
        - -database=sqlite3:///data/appdata.db
        - up
        volumeMounts:
        - name: migrations
          mountPath: /migrations
        - name: database
          mountPath: /data
      volumes:
      - name: migrations
        configMap:
          name: db-migrations
      - name: database
        persistentVolumeClaim:
          claimName: my-app-db-storage
```

## Troubleshooting

### Common Issues

#### 1. Database Locked Errors

**Problem**: `database is locked` errors when multiple pods try to access SQLite.

**Solution**: SQLite doesn't support concurrent writes. Use `replicas: 1` in your deployment.

```yaml
spec:
  replicas: 1  # Important: Keep this as 1 for SQLite
```

#### 2. Permission Denied

**Problem**: Application can't read/write to database file.

**Solution**: Check file permissions and ensure the container runs with appropriate user:

```yaml
spec:
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
```

#### 3. Database File Not Found

**Problem**: Application can't find the database file.

**Solution**: Verify the correct path and PVC name:

```bash
# Check if PVC exists
kubectl get pvc

# Check the correct database path
kubectl exec -it <sqlite-pod> -- ls -la /data/

# Verify the database file name matches your spec
```

#### 4. Storage Class Not Found

**Problem**: PVC stuck in `Pending` state due to missing storage class.

**Solution**: Check available storage classes:

```bash
kubectl get storageclass

# Use a valid storage class or omit for default
```

### Debugging Commands

```bash
# Check SQLiteDB status
kubectl get sqlitedb
kubectl describe sqlitedb my-app-db

# Check PVC status
kubectl get pvc
kubectl describe pvc my-app-db-storage

# Check database contents
kubectl exec -it <sqlite-pod> -- sqlite3 /data/appdata.db ".tables"
kubectl exec -it <sqlite-pod> -- sqlite3 /data/appdata.db ".schema"

# Check file permissions
kubectl exec -it <sqlite-pod> -- ls -la /data/
```

### Performance Considerations

1. **Single Writer**: SQLite only supports one concurrent writer
2. **Read Replicas**: Consider read-only replicas for read-heavy workloads
3. **Storage Performance**: Use SSD storage classes for better I/O performance
4. **Database Size**: Monitor database size and plan storage accordingly
5. **Backup Strategy**: Implement regular backups for production databases

### Best Practices

1. **Use appropriate storage classes** for your performance requirements
2. **Set resource limits** on your application pods
3. **Implement health checks** to ensure database connectivity
4. **Use init containers** to wait for database readiness
5. **Monitor database size** and performance metrics
6. **Plan for database migrations** in your application lifecycle
7. **Implement proper error handling** for database operations
8. **Use connection pooling** where appropriate (though less critical for SQLite)

## Additional Resources

- [SQLite Documentation](https://sqlite.org/docs.html)
- [Kubernetes Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)
- [Storage Classes](https://kubernetes.io/docs/concepts/storage/storage-classes/)
- [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
