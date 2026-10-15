# Workspaces in the browser

A `GryviaWorkspace` is a Jupyter or VS Code (code-server) pod with optional GPUs and a volume that survives pause and
resume; the controller is described in [ML controllers](ml-controllers.md#gryviaworkspace). This page covers opening a
workspace from the browser through the api-gateway (the **workspace proxy**), the CLI, and idle detection.

**Status:** unit-tested (operator: Go tests with a fake client and a stand-in HTTP server; gateway: pytest with a mock
transport and a mock WebSocket upstream; CLI: Rust unit tests), and run once end to end on a kind cluster
(`GRYVIA_LOCAL_IMAGES=1 ./scripts/kind-demo.sh`, CPU workspace with `quay.io/jupyter/minimal-notebook`) through the
dashboard's nginx and the gateway: launch link and cookie, JupyterLab and its API, a kernel running code over the
WebSocket, refusal without a cookie or with a tampered token, `status.lastActivity` following Jupyter's own activity,
and the NetworkPolicy blocking a pod in another namespace (kind's kindnet enforces it). code-server and GPU workspaces
have not been run.

## Turn it on

```bash
helm upgrade --install gryvia ./helm/gryvia -n gryvia-system \
  --set workspaceProxy.enabled=true \
  --set workspaceProxy.publicURL=https://gryvia.example.com
```

`publicURL` is the HTTPS address browsers use for the dashboard (the `gryvia-ui` Service or the ingress in front of
it); the dashboard's nginx forwards `/ws/` to the gateway, including WebSockets. The chart generates the Secret
`gryvia-workspace-proxy` (key `key`) once and keeps it across upgrades; set `workspaceProxy.existingSecret` to bring
your own (at least 32 bytes). The ai-operator reads it with `--workspace-proxy-key-file`, the gateway as
`GRYVIA_WORKSPACE_PROXY_KEY`. The operator also takes `--workspace-public-url` and `--platform-namespace`.

The kind demo turns it on when it builds images from the checkout:
`GRYVIA_LOCAL_IMAGES=1 ./scripts/kind-demo.sh` creates the CPU workspace `demo-notebook`
([examples/demo/workspace.yaml](../examples/demo/workspace.yaml)).

## How a browser gets in

1. The user clicks **Open** on the dashboard's Workspaces page, or runs `gryvia workspace open <name>`. Either calls
   `POST /api/workspaces/{name}/open` with the usual gateway authentication, so tenants only reach workspaces in their
   own namespace. The workspace must be `Running` or `Idle`.
2. The reply is a launch link `<publicURL>/ws/<namespace>/<name>/?gryvia_launch=<token>`, signed with the shared key,
   bound to the workspace's UID and valid for 120 seconds. Anyone who opens it in that time gets in, so it is not
   something to share.
3. The first request checks the token, sets the cookie `gryvia_ws` (scoped to `/ws/<namespace>/<name>/`, `HttpOnly`,
   `Secure`, `SameSite=Strict`, 8 hours) and redirects to the same path without the token.
4. Later HTTP requests and WebSockets under that path are proxied to the workspace Service. A missing or expired
   cookie gets a 401 page; a WebSocket is closed with code 4401.

Recreating a workspace with the same name changes its UID, which invalidates old links and cookies.

## What the operator changes when the proxy is on

| | Jupyter | VS Code (code-server) |
| --- | --- | --- |
| Authentication | `JUPYTER_TOKEN` is an HMAC of the shared key, namespace, name and UID; the gateway derives the same token and sends it upstream, so users never see it | `--auth none`, only for the operator's default image (a custom `spec.image` keeps its own arguments) |
| Path | `NOTEBOOK_ARGS=--ServerApp.base_url=/ws/<ns>/<name>/` (honoured by Jupyter docker-stacks images); the gateway keeps the full path | The gateway strips the prefix |
| Readiness probe | `/ws/<ns>/<name>/api` | `/healthz` |

`status.url` becomes `<publicURL>/ws/<namespace>/<name>/`. A NetworkPolicy `<name>-workspace` admits traffic to the pod
only from the gateway (`app=gryvia-api-gateway`) and the ai-operator in the platform namespace. NetworkPolicies are
enforced only by CNIs that implement them; on a CNI that does not, anything in the cluster that can reach the Service
can still reach a VS Code workspace without a password, and a Jupyter workspace with its token.

Without the proxy nothing changes: the URL is the in-cluster Service and there is no authentication in front of it.

## Idle detection

With the proxy on, the operator asks the server itself when it was last used, at least once a minute while the
workspace runs: Jupyter's `/api/status` (`last_activity`, with the token) or code-server's `/healthz` (`lastHeartbeat`). The
newest of that, the session start and the `gryvia.io/last-activity` annotation is `status.lastActivity`; after
`spec.idleTimeoutMinutes` without activity the phase becomes `Idle`, and with the annotation
`gryvia.io/idle-action: pause` the workspace is paused (pod deleted, volume kept). A server that cannot be reached
counts as no new activity, and an `Idle` workspace that is used again goes back to `Running`. Requests time out after
3 seconds and read at most 64 KiB.

## CLI

```bash
gryvia workspace new nb --type jupyter --gpus 1 --storage 50Gi --idle-timeout 60 --idle-pause -n team-a
gryvia workspace list -n team-a
gryvia workspace open nb            # needs GRYVIA_GATEWAY_URL (and GRYVIA_API_KEY); --print to only print the link
gryvia workspace pause nb -n team-a
gryvia workspace resume nb -n team-a
gryvia workspace delete nb -n team-a --yes   # also deletes the volume and its data
```

`get` and `create -f <file>` work as for the other kinds; `ws` is an alias of `workspace`.

## Default images

Without `spec.image` the operator uses `quay.io/jupyter/pytorch-notebook:cuda12-latest` (Jupyter docker-stacks, CUDA
12 and PyTorch; several GB) or `codercom/code-server:latest`. Both are moving tags; pin a digest with
`--workspace-jupyter-image` and `--workspace-code-image` (chart `aiOperator.extraArgs`) for reproducible workspaces.
