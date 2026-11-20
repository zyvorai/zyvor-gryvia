# Authentication and TLS

## Signing in

The dashboard and API use one shared bearer key. Sign in as **`admin`** with the key as the password.
The dashboard sends the credentials to `POST /api/auth/login`; the gateway checks them and returns a signed,
short-lived session token (8 hours by default) that the browser uses for later requests. The web UI contains
no credential and the long-lived API key is never stored in the browser. Sessions end at expiry (the dashboard
returns you to the sign-in page) and also when the API key is rotated. Tune them on the gateway with
`GRYVIA_SESSION_TTL_SECONDS`, and set `GRYVIA_SESSION_SECRET` to sign sessions with a key of their own.

### Finding the password

The sign-in page says where the key is stored and shows the command that prints it, with the Secret and namespace of
your install (from `GET /api/auth/config`; the page never shows the key itself). With the chart defaults:

```bash
kubectl -n gryvia-system get secret gryvia-api-key -o jsonpath='{.data.GRYVIA_API_KEY}' | base64 -d
```

With `auth.existingSecret`, the page names that Secret instead. `scripts/deploy-remote.sh` also keeps a copy in
`~/.gryvia/api-key` on the host.

Scripts and `curl` can skip the login and send the key directly:

```bash
curl -k -H "Authorization: Bearer $GRYVIA_API_KEY" https://<host>/api/cluster/stats
```

### The default key

Installs default to the well-known lab key `Admin@321` so a quick start needs no setup. While it is in
use the dashboard shows a dismissible warning. Before exposing Gryvia to anyone else pick one of:

```bash
# 1. Your own key
helm upgrade --install gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system \
  --set auth.apiKey='a-long-random-secret'

# 2. Let the chart generate a random key once and keep it across upgrades
helm upgrade --install gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system --set auth.apiKey=
kubectl -n gryvia-system get secret gryvia-api-key -o jsonpath='{.data.GRYVIA_API_KEY}' | base64 -d

# 3. A Secret you manage (key: GRYVIA_API_KEY)
helm upgrade --install gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system \
  --set auth.existingSecret=my-gryvia-key
```

`./scripts/install.sh` reads `GRYVIA_API_KEY` from the environment (default: the lab key). `./scripts/deploy-remote.sh`
reads it too, but when it is unset a redeploy **keeps the key that is already installed**: the `gryvia-api-key` Secret,
then `~/.gryvia/api-key` on the host. The lab default is used only on a first install, so rotating the key with
`helm upgrade --set auth.apiKey=...` is not undone by the next deploy. The key in use is saved to `~/.gryvia/api-key`
(mode 600), and the script prints it only when it is the lab default.

### Rate limiting

Sign-in is limited to 10 attempts per minute per client address, and a failed attempt is delayed by half
a second. Behind the dashboard's proxy the gateway uses the forwarded client address, and it trusts that
header only when the connection comes from a private or loopback address.

### Single sign-on

The gateway can validate OIDC JWTs issued by your identity provider. The dashboard then offers an SSO
button next to the key login. Signed-in OIDC users are **tenant users**: they only see their own tenant's data
(see [GPU as a Service](./GPU_AS_A_SERVICE.md)). The API key and browser sessions are the provider **admin**.

| Variable | Meaning |
| --- | --- |
| `OIDC_ENABLED` | `true`, `1` or `yes` turns OIDC on. Default is off. |
| `OIDC_ISSUER_URL` | Issuer URL (a trailing `/` is removed). The gateway reads `<issuer>/.well-known/openid-configuration` and the `jwks_uri` it lists. OIDC stays off when this is empty. |
| `OIDC_CLIENT_ID` | Client id handed to the dashboard through `/api/auth/config`. |
| `OIDC_AUDIENCE` | Required `aud` of the token. Defaults to `OIDC_CLIENT_ID`. |

A bearer token with three dot-separated parts is checked as a JWT: RS256/384/512 or ES256/384 signature
against the provider's JWKS (matched by `kid`), the issuer, the audience, and the presence of `exp`,
`iss`, `aud` and `sub`. Signing keys are cached for one hour and refreshed when an unknown `kid`
appears, at most once every 30 seconds so unauthenticated requests cannot make the gateway hammer the
provider (after a key rotation, a token with the new key is accepted once that interval has passed). A token that fails is then tried as the API key, so it is rejected with 403 unless it equals
that key; if the provider cannot be reached the gateway does not accept the token. Signed browser
sessions (`gs1.` tokens) and the API key keep working while OIDC is on. `/api/auth/config` reports
`oidcEnabled: false` with an error when discovery fails.

Roles and tenants: an OIDC token is a tenant user unless its `groups` claim contains one of
`GRYVIA_OIDC_ADMIN_GROUPS` (comma list, chart value `apiGateway.oidc.adminGroups`; default none, so nobody is
admin through OIDC). The tenant comes from the `org` claim (a string) or else the `groups` entries, matched against
`GryviaTenant` names (or their `tenant-<name>` namespace). Namespaces always come from the matched tenants, so a token
cannot name an arbitrary namespace, and a token that matches no tenant gets 403. Only while no `GryviaTenant` exists
can `GRYVIA_OIDC_LEGACY_NAMESPACES=1` (`apiGateway.oidc.legacyNamespaces`) keep the old "claim is a namespace"
behaviour for installs that predate tenants. `/api/auth/me` returns `sub`, `email`, `name` (or
`preferred_username`), `groups`, `org`, `role`, `tenant`, `tenants` and `tenantNamespaces`. Your provider must include
`groups` (or `org`) in the token; the dashboard requests the scopes `openid profile email groups`.

## TLS

The dashboard (port 443) and the gateway (port 8080) serve HTTPS. `tls.mode` in the chart selects the
certificate source:

| Mode | Behaviour |
|------|-----------|
| `selfSigned` (default) | The chart creates one self-signed certificate in the `gryvia-tls` Secret and reuses it on upgrades. Add names or IPs with `tls.extraSANs`. Browsers warn once. |
| `certManager` | Creates a cert-manager `Certificate` for the issuer in `tls.certManager.issuerName` (`issuerKind` defaults to `ClusterIssuer`). |
| `existingSecret` | Uses a `kubernetes.io/tls` Secret you created, named by `tls.secretName`. |

To rotate the self-signed certificate, delete the `gryvia-tls` Secret and run `helm upgrade`.

To publish the dashboard under a real hostname enable the Ingress and let it terminate TLS:

```bash
helm upgrade --install gryvia oci://ghcr.io/zyvorai/charts/gryvia -n gryvia-system \
  --set ui.ingress.enabled=true --set ui.ingress.className=nginx \
  --set ui.ingress.host=gryvia.example.com --set ui.ingress.tlsSecretName=gryvia-example-com \
  --set 'ui.ingress.annotations.nginx\.ingress\.kubernetes\.io/backend-protocol=HTTPS'
```

The dashboard proxy talks to the gateway over HTTPS inside the cluster using the shared certificate and does
not verify it. On shared clusters enable `--set networkPolicy.enabled=true` so only the dashboard pods can
reach the gateway.
