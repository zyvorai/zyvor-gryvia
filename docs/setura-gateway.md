# Serving the dashboard through Setura

The dashboard can be exposed with the Kubernetes [Gateway API](https://gateway-api.sigs.k8s.io/) on
[Setura](https://github.com/zyvorai/zyvor-setura-gateway) (a Gateway API controller with a built-in proxy)
instead of an Ingress controller. The chart renders a `Gateway`, an app `HTTPRoute` and a `BackendTLSPolicy`;
the raw manifests (`manifests/deploy/ui-deployment.yaml`, MLflow, VS Code) use the same objects.

## Install

```bash
GRYVIA_GATEWAY_HOST=gryvia.example.com ./scripts/install.sh
```

`install.sh` first runs `scripts/install-setura-gateway.sh`, which installs Setura and the `setura`
GatewayClass **only if the cluster has no Setura yet** (a GatewayClass with controller
`zyvor.dev/setura-gateway`); on a cluster that already has it, it does nothing. Then it installs the chart with
`ui.gateway.enabled=true` and creates the `gryvia-ui-ca` ConfigMap (from the `gryvia-tls` Secret) that Setura
uses to verify the dashboard's HTTPS backend.

The script needs the `setura` CLI on PATH and runs the equivalent of:

```bash
setura gateway install --image ghcr.io/zyvorai/zyvor-setura-gateway:0.7.0 --namespace setura-system | kubectl apply -f -
```

It also applies the standard Gateway API CRDs (`GATEWAY_API_VERSION`, default v1.4.0) when the cluster has none.

For HTTPS on the client side add `GRYVIA_GATEWAY_TLS_SECRET=<kubernetes.io/tls secret>`.

## Ports

| Listener | Port | Value |
|---|---|---|
| HTTP | 80 (redirects to HTTPS when a certificate is set) | fixed |
| HTTPS | **7443** | `ui.gateway.httpsPort` |

The HTTPS port is deliberately not 443; change it with `--set ui.gateway.httpsPort=<port>`. The redirect
route sends browsers to that port. Setura's generated Service exposes ports 80 and 443 only: for 7443 install
with `setura gateway install --expose-gateway-ports` (the controller then adds a Service port for every
listener port) or add the port to the `setura-gateway` Service yourself.

## Notes

- The dashboard pod still serves its static assets with nginx; Setura replaces the Ingress controller, not the
  file server inside the pod.
- Without a LoadBalancer implementation (plain k3s) the Gateway address comes from the `setura-gateway`
  Service ClusterIP; reach it through the Service's NodePort or a port-forward
  (`kubectl -n setura-system get svc setura-gateway`).
- Do not enable `ui.ingress` and `ui.gateway` together; the chart refuses it.
