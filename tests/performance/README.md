# Gryvia Performance Benchmarks

Performance benchmarking tools for Gryvia platform.

> **Status.** `benchmark.go` is a manual load generator, not a validated benchmark suite. It uses the standard kubeconfig loading rules (`$KUBECONFIG` or `~/.kube/config`) and creates real `GryviaAIJob` objects (1 GPU of type H100 each, in a hard-coded namespace) in whatever cluster it points at, so it needs a cluster with GPUs and quota, and it should not be pointed at a production cluster. There are no published results: the JSON below and the "Performance Targets" are illustrative design targets, not measurements of this project. It is not run in CI.

## Benchmarks

### 1. Sequential Job Submission
Submits 10 jobs one at a time.

### 2. Parallel Job Submission
Submits 20 jobs with a parallelism of 5.

### 3. Burst Submission
Submits 50 jobs at once.

### 4. Large-Scale Submission
Submits 100 jobs with a parallelism of 10.

## Running Benchmarks

```bash
cd tests/performance
go run benchmark.go
```

## Results

Results are saved to `benchmark_results.json`. The values below are an example of the format only:

```json
[
  {
    "test": "Sequential Submission",
    "total_jobs": 10,
    "successful_jobs": 10,
    "failed_jobs": 0,
    "avg_time_to_schedule_ms": 2500,
    "avg_time_to_complete_ms": 15000,
    "total_duration_ms": 180000,
    "jobs_per_second": 0.055
  }
]
```

## Metrics

- **Total Jobs**: Number of jobs submitted
- **Successful Jobs**: Jobs that completed successfully
- **Failed Jobs**: Jobs that failed
- **Avg Time to Schedule**: Average time from submission to running state
- **Avg Time to Complete**: Average total execution time
- **Total Duration**: Total benchmark duration
- **Jobs/Second**: Job submission throughput

## Performance Targets

Aspirational targets, not measured or guaranteed (nobody has established that Gryvia meets them):

- **Time to Schedule**: < 5 seconds
- **Jobs/Second**: > 10 jobs/second
- **Success Rate**: > 95%
- **Large Scale (100 jobs)**: Complete in < 10 minutes

## Customization

Edit `benchmark.go` to customize:
- Number of jobs per test
- Parallelism levels
- Job specifications
- Timeout values

## CI/CD Integration

The benchmarks are not run automatically in CI; there is no nightly job or trend tracking today.

## Interpreting Results

Good performance:
```
Avg Time to Schedule: < 5s
Jobs/Second: > 10
Success Rate: 100%
```

Needs investigation:
```
Avg Time to Schedule: > 30s
Jobs/Second: < 1
Success Rate: < 90%
```

Common issues:
- High schedule time: Quota limits, node availability
- Low throughput: API server limits, operator performance
- Failures: Resource constraints, configuration errors
