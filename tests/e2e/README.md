# Gryvia E2E Tests

End-to-end tests for Gryvia platform.

> **Status.** These four Go tests (`tests/e2e/e2e_test.go`) run against whatever cluster your default kubeconfig points at (`$KUBECONFIG` if set, otherwise `~/.kube/config`) and expect the CRDs and operators to be installed. They are **not run in CI**: no workflow under `.github/workflows` invokes `tests/e2e`. The CI job that exercises a real install is `kind-e2e.yml` (kind cluster with demo data, no GPUs, API and CLI checks), which is a separate thing. The GPU node test only logs and asserts on nodes that already exist. Nothing here has been run against real GPU, VAST or RDMA hardware.

## Prerequisites

- Running Kubernetes cluster
- Gryvia operators deployed
- `kubectl` configured
- Go (the module in `tests/e2e/go.mod` declares the required version)

## Running Tests

### Run all E2E tests

```bash
cd tests/e2e
go test -v -timeout 30m ./...
```

### Run specific test

```bash
go test -v -run TestE2E_AIJobLifecycle
go test -v -run TestE2E_QuotaEnforcement
go test -v -run TestE2E_NodeRegistration
go test -v -run TestE2E_StorageProvisioning
```

### Run with parallel execution

```bash
go test -v -parallel 4 ./...
```

## Test Scenarios

### 1. AI Job Lifecycle Test
Tests complete job lifecycle:
- Create GryviaAIJob
- Wait for the job status to be set by the operator
- Verify the status
- Clean up job

### 2. Quota Enforcement Test
Tests quota management:
- Create team quota
- Verify quota status updates
- Check quota limits
- Delete quota

### 3. Node Registration Test
Tests GPU node management:
- List all GPU nodes
- Verify node specifications
- Check GPU counts and types

### 4. Storage Provisioning Test
Tests storage backend:
- Create storage configuration
- Wait for CSI driver deployment
- Verify storage ready status
- Clean up storage

## Test Environment

Each test creates its own namespace named `gryvia-e2e-<test-name>` and deletes it when the test ends. Cluster-scoped objects (quota, storage) are deleted by the test that creates them; a failed run may leave some behind.

## CI/CD Integration

These tests are not wired into GitHub Actions today, so they run only when you run them by hand. See `.github/workflows/kind-e2e.yml` for the install-path check that does run on pull requests.

## Troubleshooting

### Tests fail to connect to cluster
The tests use the standard kubeconfig loading rules: `$KUBECONFIG` if set, otherwise `~/.kube/config`. Make sure the current context points at the target cluster.

### Timeout errors
Increase timeout:
```bash
go test -v -timeout 60m ./...
```

### Check test logs
```bash
go test -v -timeout 30m ./... 2>&1 | tee test.log
```

## Writing New Tests

Example test structure:

```go
func TestE2E_NewFeature(t *testing.T) {
    setupTestCluster(t)
    defer teardownTestCluster(t)

    ctx := context.Background()

    t.Run("SubTest1", func(t *testing.T) {
        // Test logic
        require.NoError(t, err)
        assert.Equal(t, expected, actual)
    })
}
```

## Performance Tests

For performance testing, see `tests/performance/`.
