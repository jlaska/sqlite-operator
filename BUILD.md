# Build and CI/CD Setup Guide

This guide covers setting up the build pipeline and CI/CD for the SQLite Operator with Quay.io integration.

## Prerequisites

1. **Quay.io Account**: Create an account at [quay.io](https://quay.io)
2. **GitHub Repository**: Push your code to GitHub
3. **Container Registry**: Create a repository at `quay.io/jlaska/sqlite-operator`

## Quay.io Setup

### 1. Create Repository

1. Go to [quay.io](https://quay.io)
2. Click "Create New Repository"
3. Name: `sqlite-operator`
4. Visibility: Public (or Private if preferred)
5. Click "Create Public Repository"

### 2. Generate Robot Account (Recommended)

1. Go to your repository settings
2. Click "Robot Accounts" 
3. Create a new robot account with "Write" permissions
4. Note the username (e.g., `jlaska+sqlite_operator_bot`) and token

### 3. Alternative: Use Personal Credentials

You can use your personal Quay.io username and password, but robot accounts are more secure.

## GitHub Actions Setup

### 1. Add Secrets to GitHub Repository

Go to your GitHub repository → Settings → Secrets and variables → Actions

Add these secrets:

```
QUAY_USERNAME=jlaska+sqlite_operator_bot  # Your robot account username
QUAY_PASSWORD=<robot-account-token>       # Your robot account token
```

Or if using personal credentials:
```
QUAY_USERNAME=jlaska              # Your Quay.io username  
QUAY_PASSWORD=<your-password>     # Your Quay.io password
```

### 2. Repository Settings

Ensure these settings are enabled:
- Actions → General → Allow all actions and reusable workflows
- Actions → General → Allow GitHub Actions to create and approve pull requests

## Local Development Workflow

### Building Locally

```bash
# Build and test locally
make ci-build

# Build for specific version
make docker-build IMG=quay.io/jlaska/sqlite-operator:v0.1.0

# Push to registry
make docker-push IMG=quay.io/jlaska/sqlite-operator:v0.1.0

# Generate release manifests
make ci-deploy-manifests IMG=quay.io/jlaska/sqlite-operator:v0.1.0
```

### Version Management

```bash
# Check current version
make version

# Bump versions
make version-bump-patch  # v0.1.0 → v0.1.1
make version-bump-minor  # v0.1.0 → v0.2.0  
make version-bump-major  # v0.1.0 → v1.0.0

# Build with new version
make docker-build
make docker-push
```

### Multi-Architecture Builds

```bash
# Build for multiple architectures
make docker-build-multiarch IMG=quay.io/jlaska/sqlite-operator:v0.1.0
```

## CI/CD Pipeline Workflow

### Automatic Triggers

The pipeline runs on:
- **Push to main/develop**: Builds and pushes `latest` tag
- **Push tags (v*)**: Builds, pushes versioned image, creates GitHub release
- **Pull Requests**: Runs tests and security scans

### Pipeline Stages

1. **Test**: Runs Go tests and linting
2. **Build**: Builds multi-arch container image and pushes to Quay
3. **Security Scan**: Scans image with Trivy for vulnerabilities  
4. **Generate Manifests**: Creates deployment YAML files
5. **Release** (tags only): Creates GitHub release with manifests

### Manual Releases

To create a release:

```bash
# Bump version and commit
make version-bump-minor
git add Makefile.versions
git commit -m "bump: version to v0.2.0"

# Create and push tag
git tag v0.2.0
git push origin v0.2.0
```

This will trigger the full pipeline and create a GitHub release.

## Deployment Options

### Option 1: Direct kubectl

```bash
# Deploy operator
kubectl apply -f https://github.com/jlaska/sqlite-operator/releases/latest/download/sqlite-operator.yaml

# Deploy samples
kubectl apply -f https://github.com/jlaska/sqlite-operator/releases/latest/download/samples.yaml
```

### Option 2: Kustomize

```bash
# Clone and deploy
git clone https://github.com/jlaska/sqlite-operator.git
cd sqlite-operator
kubectl apply -k deploy/
kubectl apply -k deploy/samples/
```

### Option 3: ArgoCD

Create an ArgoCD Application:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: sqlite-operator
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/jlaska/sqlite-operator.git
    targetRevision: HEAD
    path: deploy
  destination:
    server: https://kubernetes.default.svc
    namespace: sqlite-operator-system
  syncPolicy:
    automated: {}
    syncOptions:
    - CreateNamespace=true
```

## Monitoring Build Status

### GitHub Actions
- View workflow runs at: `https://github.com/jlaska/sqlite-operator/actions`

### Quay.io 
- View builds at: `https://quay.io/repository/jlaska/sqlite-operator`
- Check tags and security scans

### Releases
- View releases at: `https://github.com/jlaska/sqlite-operator/releases`

## Troubleshooting

### Build Failures

```bash
# Check workflow logs in GitHub Actions
# Common issues:
# 1. Invalid Quay credentials
# 2. Repository permissions  
# 3. Docker build errors

# Test locally:
make ci-build
```

### Image Pull Issues

```bash
# Verify image exists
docker pull quay.io/jlaska/sqlite-operator:latest

# Check repository visibility
# Ensure Quay repository is public or credentials are configured
```

### Deployment Issues

```bash
# Check operator logs
kubectl logs -n sqlite-operator-system deployment/sqlite-operator-controller-manager

# Verify image reference in deployment
kubectl get deployment -n sqlite-operator-system -o yaml | grep image
```

## Security Best Practices

1. **Use Robot Accounts**: More secure than personal credentials
2. **Limit Permissions**: Robot accounts should only have necessary permissions
3. **Regular Updates**: Keep dependencies updated via Dependabot
4. **Vulnerability Scanning**: Pipeline includes Trivy security scanning
5. **Signed Images**: Consider enabling image signing with cosign

## Next Steps

1. **Push to GitHub**: Commit and push your changes
2. **Verify Pipeline**: Check that GitHub Actions runs successfully  
3. **Test Deployment**: Deploy to a test cluster
4. **Production Deployment**: Use ArgoCD or your preferred GitOps tool