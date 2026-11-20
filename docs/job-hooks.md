# Job hooks

A `GryviaJobHook` sends an HTTP POST when a `GryviaAIJob` or `GryviaWorkflow` in its namespace reaches one of the
hook's events, for example `Failed` or `Succeeded`. Use it to post to Slack, open a ticket, or start your own
automation (push a model, kick off an evaluation) when training ends. The hook only notifies: it does not run code in
the cluster.

The ai-operator delivers the POST. Bodies can be signed, failed POSTs are retried with backoff, and receivers on
private addresses are refused unless the operator allows them.

The feature is opt-in.

## What is verified and what is not

| Verified | How |
| --- | --- |
| Matching by event, kind and label selector; suspended and invalid hooks skipped; one delivery per transition, again for a new transition (a retried job, the next run of a scheduled workflow); no backfill of transitions older than the hook; retries with doubling backoff, then giving up; the signing Secret (a missing one counts as a failed attempt); Slack format; delivery records pruned when a hook is deleted; the Ready condition | Fake-client tests in `operators/ai-operator/controllers/gryviajobhook_controller_test.go` |
| The address guard (loopback, RFC 1918, CGNAT, link-local and metadata, multicast, reserved, IPv6 ULA; allowed CIDRs; a public name resolving to a private address; plain http only to allowed CIDRs), refused redirects, signature and headers | `operators/ai-operator/pkg/jobhook/deliver_test.go` against a local HTTP server |
| Gateway routes (validation, header values never returned, suspend, tenant scoping) and the CLI table | `services/api-gateway/tests/test_jobhooks.py`, `cli/src/commands/jobhooks.rs` |
| The delivery workers: reconciles return while receivers are slow, a slow receiver does not hold up another hook, a delivery queued or in flight is not queued twice, a full queue retries the job after a second, a finished delivery enqueues its job again, and the outcome of a transition the job has left is not recorded | `operators/ai-operator/controllers/gryviajobhook_controller_test.go` (`go test -race`) |
| A real cluster: a stand-in receiver gets a signed delivery for a failed workflow and for a succeeded one, the hook's counters move, and a hook pointed at a non-allowed address records a refused attempt | The "Job hooks" steps of `.github/workflows/e2e-ml.yml`; these steps also passed on a single-node k3s host (2026-10-02) |

| Not verified | Why |
| --- | --- |
| Slack itself, or any other third-party receiver | The e2e receiver is a stand-in that checks the signature |
| Exactly-once delivery | Delivery is at least once (see Delivery); receivers should deduplicate on `X-Gryvia-Delivery` |
| Many hooks or a high job churn | Deliveries are POSTed by a bounded pool of workers (below); not load-tested |

## Turning it on

```bash
helm upgrade gryvia ./helm/gryvia -n gryvia-system --reuse-values \
  --set aiOperator.jobHooks.enabled=true
```

This adds `--enable-job-hooks` to the ai-operator. Receivers inside the cluster have private addresses, which are
refused by default. To allow them, list their CIDRs, usually the cluster's service CIDR (and the pod CIDR if you post
to pod IPs):

```bash
helm upgrade gryvia ./helm/gryvia -n gryvia-system --reuse-values \
  --set aiOperator.jobHooks.enabled=true \
  --set-json 'aiOperator.jobHooks.allowedCIDRs=["10.43.0.0/16"]'
```

Plain `http://` URLs are only dialed to those CIDRs; anything else needs `https://`.

## A hook

```yaml
apiVersion: gryvia.io/v1alpha1
kind: GryviaJobHook
metadata:
  name: publish-model
  namespace: ml-research
spec:
  events: [Succeeded]          # Running, Succeeded, Failed, Cancelled, Preempted, Rejected
  kinds: [GryviaAIJob]         # GryviaAIJob, GryviaWorkflow; empty = both
  selector:                    # optional label selector on the jobs
    matchLabels:
      team: nlp
  webhook:
    url: https://automation.example.com/gryvia/job-succeeded
    format: gryvia             # gryvia (default) or slack
    headers:                   # optional, non-secret
      X-Team: nlp
    secretRef:                 # optional: the Secret's "secret" key signs each body
      name: hook-secret
    timeoutSeconds: 10         # 1-30, default 10
  retry:
    attempts: 3                # POSTs per transition, including the first; 1-10, default 3
    backoffSeconds: 10         # before the second attempt; doubles each time, capped at one hour
  suspend: false
```

Workflows report `Running`, `Succeeded` and `Failed`; AI jobs report all six events. More examples are in
[examples/crds/gryviajobhook-examples.yaml](../examples/crds/gryviajobhook-examples.yaml).

Header values are stored in the hook's spec, readable by anyone who can read the hook. Do not put tokens there; use
`secretRef` and verify the signature instead. The gateway and the CLI table show header names only.

`status` reports a `Ready` condition (`Valid`, `Suspended`, `InvalidSpec`, `SecretMissing`), `deliveries` (successful
POSTs), `failures` (transitions given up on after the last attempt) and `lastDelivery` (job, event, attempt, result
and error).

## The request

```
POST <url>
Content-Type: application/json
User-Agent: gryvia-job-hook
X-Gryvia-Event: Failed
X-Gryvia-Delivery: 3f0c...            (the same for every attempt of one transition)
X-Gryvia-Timestamp: 1790000000
X-Gryvia-Signature: sha256=<hex>      (only with secretRef)
```

```json
{
  "hook": "publish-model",
  "event": "Failed",
  "kind": "GryviaAIJob",
  "namespace": "ml-research",
  "name": "llama-ft",
  "uid": "8b0d...",
  "message": "OOMKilled",
  "labels": {"team": "nlp"},
  "time": "2026-10-02T09:59:00Z",
  "delivery": "3f0c...",
  "attempt": 1
}
```

`run` is added for workflows (the run number of a scheduled workflow). `time` is when the job entered the phase, when
known. `message` is cut at 1000 bytes. With `format: slack` the body is `{"text": "GryviaAIJob ml-research/llama-ft
Failed: OOMKilled"}`, which a Slack incoming webhook accepts.

The signature is the same as the budget and invoice webhooks': `sha256=` followed by the hex HMAC-SHA256 of
`<X-Gryvia-Timestamp>.<body>` with the Secret's `secret` key. Reject old timestamps.

```python
import hashlib, hmac, time

def verify(secret: bytes, timestamp: str, body: bytes, signature: str) -> bool:
    if abs(time.time() - int(timestamp)) > 300:
        return False
    want = "sha256=" + hmac.new(secret, timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, signature)
```

A 2xx answer is a success. Anything else, a timeout or a refused address is a failed attempt. Redirects are not
followed.

## Delivery

- **When.** The operator watches AI jobs and workflows. When one is in a phase a hook lists, and the hook has not
  delivered that transition yet, it POSTs. A transition is the phase together with the job's start time (and the run
  number for workflows), so a retried job or the next run of a scheduled workflow is delivered again.
- **No backfill.** A transition that happened before the hook was created is not delivered. Transitions that happen
  while a hook is suspended are not delivered later.
- **At least once.** The operator records each hook's state for the job in the job's `gryvia.io/job-hooks` annotation
  after the POST. If that write fails, or the operator restarts in between, the POST is sent again with the same
  `X-Gryvia-Delivery`.
- **Retries.** A failed attempt is retried after `backoffSeconds`, then twice that, and so on, until `attempts` POSTs
  have been made. Then the transition counts in `status.failures` and is not retried.
- **Workers.** The operator's job reconciles only decide which deliveries are due and queue them. `aiOperator.jobHooks.workers`
  (`--job-hook-workers`, default 8) goroutines POST them, record the outcome on the job, and have the job looked at
  again. A slow receiver holds one worker for at most its `timeoutSeconds`. When the queue (256 deliveries) is full,
  the job is looked at again after a second.
- **Missed phases.** A hook sees the phase the job is in when the operator reconciles it. A job that passes through a
  phase faster than that (for example `Running` for a job that fails at once) may not be seen in it.

## Addresses the operator refuses

Hook URLs are written by tenants and called from the operator's pod, so the operator refuses to connect to:
loopback, RFC 1918 and IPv6 unique local addresses, CGNAT (100.64.0.0/10), link-local addresses (including the
169.254.169.254 metadata address), multicast, unspecified and reserved addresses. The check runs on the addresses the
operator is about to dial, after DNS resolution, so a public name that resolves to a private address is refused too.
`aiOperator.jobHooks.allowedCIDRs` lifts the restriction for the listed ranges, and only those ranges may use plain
http. URLs with user info or a fragment are rejected.

## Gateway and CLI

```bash
gryvia job-hooks create -f examples/crds/gryviajobhook-examples.yaml -n ml-research
gryvia job-hooks list -n ml-research
gryvia job-hooks get slack-failures -n ml-research
gryvia job-hooks delete slack-failures --yes
```

The gateway serves `GET/POST /api/job-hooks`, `GET/DELETE /api/job-hooks/{name}` and
`POST /api/job-hooks/{name}/suspend` (`{"suspend": true}`), scoped to the caller's tenant namespaces.
