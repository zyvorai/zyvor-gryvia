# Operations: upgrade, uninstall, backup, troubleshooting

## Upgrade

Helm installs CRDs on the first install only and never upgrades them, so apply the CRDs first:

```bash
kubectl apply --server-side --force-conflicts -f crds/        # from the release you are upgrading to
helm upgrade gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system --reset-then-reuse-values --version <new>
```

`--reset-then-reuse-values` (Helm 3.14 or later) starts from the new chart's defaults and applies the values you set
before. Do not upgrade to a newer chart with `--reuse-values`: Helm then keeps the old chart's defaults, so a setting
added since (such as `nvidia.enabled: false`) is missing, and Helm enables a sub-chart whose condition is missing. The
chart refuses such values and names the missing key. `--reuse-values` is fine for changing settings without changing
the chart version.

Without a checkout, each release also carries the CRDs as one file:

```bash
kubectl apply --server-side --force-conflicts -f https://github.com/zyvorai/gryvia/releases/download/<tag>/gryvia-crds.yaml
```

(`gryvia-crds.yaml` is the concatenation of `crds/*.yaml`, built by the release workflow, with a `.sha256` next to it.
It is attached by the release job and has not been published yet: no release has been cut with it.)

Roll back with `helm rollback gryvia <revision> -n gryvia-system`. Helm does not downgrade CRDs: the newer CRDs stay,
which is safe for objects that were valid before.

### Automated upgrade test

`scripts/upgrade-test.sh` runs this procedure end to end on a kind cluster, and the `Upgrade and rollback` workflow
(`.github/workflows/upgrade.yml`) runs it nightly, on demand and when the script changes. It installs the previous
version (the newest release if its chart can be pulled from ghcr.io, otherwise the chart and images built from
`origin/main`), creates the demo data plus a tenant and a quota, upgrades to the checkout, and asserts that every
`gryvia.io` object keeps its `spec`, `gryvia status --brief` prints `OK`, the gateway login works, `helm rollback`
leaves healthy deployments and intact objects, and that a backup, a deletion and a restore give back the same objects.
Run it yourself with `scripts/upgrade-test.sh --dry-run` (prints the steps) or, with kind and docker,
`scripts/upgrade-test.sh --base-ref origin/main~1`. It tests the kind demo configuration (GPU add-ons off), not your
values, so read the changelog and try your own upgrade on a staging cluster first.

Gryvia is alpha and `gryvia.io/v1alpha1` may change between releases; read the [changelog](https://github.com/zyvorai/gryvia/blob/main/CHANGELOG.md)
before upgrading. **Two changes are breaking: the `Fabric*` → `Gryvia*` kind rename and the move from `gryvia.io/v1` to `gryvia.io/v1alpha1`**: objects created under the old kinds
(`FabricAIJob`, ...) or the old `v1` version are not converted. Export them, change `kind:` and `apiVersion:` and re-apply, then delete the old CRDs (`scripts/rename-kinds.sh` and `scripts/rename-version.sh` document the mapping).

The self-signed certificate (`gryvia-tls`) and the API key Secret are kept across upgrades. Open dashboard tabs
reload themselves once when a new version is deployed.

## Uninstall

```bash
helm uninstall gryvia -n gryvia-system
```

CRDs, your custom resources and the `gryvia-tls` Secret are kept on purpose. To remove everything, including
data:

```bash
kubectl get crd -o name | grep '\.gryvia\.io$' | xargs kubectl delete    # deletes every Gryvia object
kubectl delete namespace gryvia-system
```

## Back up

Gryvia keeps its state in Kubernetes custom resources. Export them all:

```bash
scripts/backup-crs.sh export gryvia-backup.yaml
```

Restore with `scripts/backup-crs.sh restore gryvia-backup.yaml` (it is `kubectl apply -f`; status fields are
recomputed by the operators). The export strips `resourceVersion`, `uid`, `status` and the other fields the API
server owns, because a plain `kubectl get -o yaml` dump cannot re-create a deleted object (the API server rejects
a `resourceVersion` on create). It contains no namespaces, Secrets or CRDs: back up the `gryvia-api-key` Secret
if you generated a random key, and apply the CRDs first on a new cluster. The round trip (export, delete, restore,
compare) is part of the automated upgrade test.

## Troubleshooting

| Symptom | What to check |
|---------|---------------|
| Pods stuck in `ImagePullBackOff` | The images are pulled from `global.imageRegistry` (default `ghcr.io/zyvorai`). Check the tag exists, or set `imagePullSecrets` for a private mirror. |
| Dashboard shows "Wrong username or password." | Sign in as `admin` with the API key. Read a generated key with `kubectl -n gryvia-system get secret gryvia-api-key -o jsonpath='{.data.GRYVIA_API_KEY}' \| base64 -d`. |
| "Too many sign-in attempts" | Sign-in is limited to 10 per minute per client address. Wait a minute. |
| Browser certificate warning | Expected with the default self-signed certificate. See [Authentication and TLS](./AUTH_AND_TLS.md) to use your own. |
| A page says "Gryvia was updated" | A new version was deployed while the tab was open. Reload. |
| Nodes show phase `Failed` with "node not found" | The `GryviaGpuNode` names a Kubernetes node that does not exist (the kind demo data is fictional). Register real nodes. |
| GPU or network pages say no source is connected | They need the collectors and operators that feed them (DCGM exporter, network-intelligence, security operator). The page names what is missing. |
| A job stays `Pending` | `kubectl -n <ns> describe gryviaaijob <name>` shows the scheduling reason, for example no node with the requested GPU type or a quota limit. |
| Operators restart or lose leadership | `kubectl -n gryvia-system logs deploy/gryvia-gpu-operator` (and `-ai-`, `-quota-`). Leader election uses leases in `gryvia-system`. |

Collect logs for a bug report with `kubectl -n gryvia-system logs -l app=gryvia-api-gateway --tail=200` and the
operator deployments above; remove secrets before sharing.
