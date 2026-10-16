# Serving the dashboard through Envoy Gateway

The dashboard can be exposed with the Kubernetes [Gateway API](https://gateway-api.sigs.k8s.io/) on
[Envoy Gateway](https://gateway.envoyproxy.io/) instead of an Ingress controller. The chart renders a
`Gateway`, an app `HTTPRoute` and a `BackendTLSPolicy`; the raw manifests (`manifests/deploy/ui-deployment.yaml`,
MLflow, VS Code) use the same objects.

## Install

```bash
GRYVIA_GATEWAY_HOST=gryvia.example.com ./scripts/install.sh
```

`install.sh` first runs `scripts/install-envoy-gateway.sh`, which installs Envoy Gateway and the `eg`
GatewayClass **only if the cluster has no Envoy Gateway yet** (a GatewayClass served by
`gateway.envoyproxy.io`); on a cluster that already has it, it does nothing. Then it installs the chart with
`ui.gateway.enabled=true` and creates the `gryvia-ui-ca` ConfigMap (from the `gryvia-tls` Secret) that Envoy
uses to verify the dashboard's HTTPS backend.

For HTTPS on the client side add `GRYVIA_GATEWAY_TLS_SECRET=<kubernetes.io/tls secret>`.

## Ports

| Listener | Port | Value |
|---|---|---|
| HTTP | 80 (redirects to HTTPS when a certificate is set) | fixed |
| HTTPS | **7443** | `ui.gateway.httpsPort` |

The HTTPS port is deliberately not 443; change it with `--set ui.gateway.httpsPort=<port>`. The redirect
route sends browsers to that port.

## Notes

- The dashboard pod still serves its static assets with nginx; Envoy replaces the Ingress controller, not the
  file server inside the pod.
- Without a LoadBalancer implementation (plain k3s) the Gateway shows `Programmed=False` /
  `AddressNotAssigned` but traffic works through the Envoy Service's NodePorts
  (`kubectl -n envoy-gateway-system get svc`).
- Do not enable `ui.ingress` and `ui.gateway` together; the chart refuses it.
