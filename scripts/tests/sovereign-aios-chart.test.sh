#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Helm render tests for helm/sovereign-aios: the shared credentials, the LLM gateway key hash, Zyntra's pack, URLs,
# certificate and keys, the agents' Zyntra service token and Zyntra's policy, Netra's certificate and keys, and the
# namespace guard. Needs helm and python3 with PyYAML and
# cryptography; no cluster. The sub-charts come from sibling checkouts (scripts/sovereign-aios-deps.sh, run here when
# charts/ is missing).
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
CHART=helm/sovereign-aios

[[ -d "$CHART/charts" ]] || scripts/sovereign-aios-deps.sh >/dev/null || { echo "sovereign-aios-deps failed"; exit 1; }

render() { helm template sa "$CHART" -n "${NS:-gryvia-system}" --kube-version 1.31.0 "$@" 2>&1; }

FAILED=0
echo "namespace guard"
if render_out="$(NS=default render)"; then
  echo "  FAIL rendered outside Gryvia's namespace"; FAILED=1
elif grep -q "install sovereign-aios into Gryvia's namespace" <<<"$render_out"; then
  echo "  ok   refuses another namespace"
else
  echo "  FAIL unexpected error: $render_out"; FAILED=1
fi

check() {
  python3 - "$1" <<'EOF'
import base64, hashlib, sys, yaml
from cryptography import x509

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
failed = 0

def ok(cond, msg):
    global failed
    print(("  ok   " if cond else "  FAIL ") + msg)
    failed |= not cond

def find(kind, name=None, contains=None):
    for d in docs:
        n = d["metadata"]["name"]
        if d["kind"] == kind and (n == name or (contains and contains in n)):
            return d
    return None

creds = find("Secret", "sovereign-aios-credentials")["stringData"]
llm = creds["ZYNTRA_AI_API_KEY"]
ok(llm.startswith("gk-") and len(llm) == 67, "LLM key has the gateway's gk-<64 hex> shape")
ok(creds["GRYVIA_API_KEY"] == creds["ZYNTRA_GRAVIA_TOKEN"], "Zyntra reads Gryvia with the Gryvia API key")

h = find("Secret", "gryvia-system.platform-sovereign-aios")
ok(h is not None and h["metadata"]["namespace"] == "gryvia-llm-keys", "key hash Secret in the key namespace")
ok(h["metadata"]["labels"].get("gryvia.io/llm-key") == "true", "key hash Secret is labelled as a gateway key")
ok(h["stringData"]["hash"] == hashlib.sha256(llm.encode()).hexdigest(), "key hash is the SHA-256 of the LLM key")

gw = find("Deployment", "gryvia-api-gateway")["spec"]["template"]["spec"]["containers"][0]
ref = [e for e in gw["env"] if e["name"] == "GRYVIA_API_KEY"][0]["valueFrom"]["secretKeyRef"]["name"]
ok(ref == "sovereign-aios-credentials", "Gryvia's gateway uses the shared credentials")
ok(find("Secret", "gryvia-api-key") is None, "Gryvia's own API key Secret is not rendered")

z = find("Deployment", contains="zyntra")["spec"]["template"]["spec"]["containers"][0]
env = {e["name"]: e.get("value") for e in z["env"]}
ok(z["args"][-1] == "/etc/zyntra-pack", "Zyntra serves the mounted pack")
ok(env.get("ZYNTRA_AI_BASE_URL") == "http://gryvia-llm-gateway:8080/v1", "Zyntra's AI is the Gryvia LLM gateway")
ok(find("Service", "gryvia-llm-gateway") is not None, "the gateway Service has the name Zyntra uses")
ok(env.get("SSL_CERT_FILE") == "/etc/gryvia-tls/tls.crt", "Zyntra trusts Gryvia's certificate")
ok({"name": "sovereign-aios-credentials"} in [e.get("secretRef") for e in z["envFrom"]], "Zyntra gets the shared credentials")

pack = find("ConfigMap", "sovereign-aios-zyntra-pack")["data"]
ok(set(pack) >= {"pack.yaml", "kpis.yaml", "ontology.yaml"}, "pack ConfigMap holds the pack files")
ok("https://gryvia-api-gateway.gryvia-system.svc:8080/api/jobs" in pack["ontology.yaml"], "connectors point at the gateway")

crt = x509.load_pem_x509_certificate(base64.b64decode(find("Secret", "gryvia-tls")["data"]["tls.crt"]))
sans = crt.extensions.get_extension_for_class(x509.SubjectAlternativeName).value.get_values_for_type(x509.DNSName)
ok({"gryvia-api-gateway", "netra"} <= set(sans), "one certificate covers the gateway and Netra")

n = find("Deployment", "netra")["spec"]["template"]["spec"]
ok({"name": "tls-secret", "secret": {"secretName": "gryvia-tls"}} in n["volumes"], "Netra serves Gryvia's certificate")
tok = creds["AGENT_ZYNTRA_TOKEN"]
ok(tok.startswith("zst_") and len(tok) == 68, "agent token has Zyntra's zst_<64 hex> shape")
pol = yaml.safe_load(find("Secret", "sovereign-aios-zyntra-policy")["stringData"]["policy.yaml"])
st = pol["service_tokens"][0]
ok(st["token_sha256"] == hashlib.sha256(tok.encode()).hexdigest() and st["roles"] == ["viewer", "proposer"],
   "Zyntra's policy holds the agent token's SHA-256 with viewer and proposer")
ok(tok not in yaml.safe_dump(pol), "the policy carries the hash, not the token")
zs = find("Deployment", contains="zyntra")["spec"]["template"]["spec"]
ok({"name": "policy", "secret": {"secretName": "sovereign-aios-zyntra-policy",
    "items": [{"key": "policy.yaml", "path": "policy.yaml"}]}} in zs["volumes"], "Zyntra mounts the policy Secret")
ok(env.get("ZYNTRA_POLICY") == "/etc/zyntra/policy.yaml", "Zyntra reads the policy")
ok(find("ConfigMap", contains="zyntra-policy") is None, "no inline policy ConfigMap")

gwenv = {e["name"]: e for e in gw["env"]}
ok(gwenv["GRYVIA_ZYNTRA_URL"]["value"] == "http://zyntra:8080" and find("Service", "zyntra") is not None,
   "the console's overview reads Zyntra at its fixed Service name")
ok(gwenv["GRYVIA_ZYNTRA_TOKEN"]["valueFrom"]["secretKeyRef"] == {"name": "sovereign-aios-credentials",
   "key": "AGENT_ZYNTRA_TOKEN"}, "with the agents' viewer/proposer token")
ok(gwenv["GRYVIA_NETRA_TOKEN"]["valueFrom"]["secretKeyRef"]["key"] == "ZYNTRA_NETRA_TOKEN", "and Netra with its API key")
ok(gwenv["GRYVIA_SOVEREIGN_CA_FILE"]["value"] == "/tls/tls.crt", "verifying both against Gryvia's certificate")
ok(env.get("ZYNTRA_EXECUTE") == "apply", "Zyntra applies approved changes")
cr = find("ClusterRole", contains="zyntra-actions")
ok(cr is not None and any("gryviapriorities" in r["resources"] for r in cr["rules"]), "with RBAC for Gryvia's resources")

na = find("Secret", "sovereign-aios-netra-auth")["stringData"]
ok(na["api-key"] == creds["ZYNTRA_NETRA_TOKEN"], "Zyntra reads Netra with Netra's API key")
ok(find("Secret", "sovereign-aios-netra-ai")["stringData"]["api-key"] == llm, "Netra's AI uses the LLM key")
sys.exit(failed)
EOF
}

echo "defaults"
OUT="$(mktemp)"
render >"$OUT" || { echo "  FAIL render"; cat "$OUT"; exit 1; }
check "$OUT" || FAILED=1

echo "policy merge and agent namespaces"
VALS="$(mktemp)"
cat >"$VALS" <<'YAML'
agentToken:
  roles: [viewer]
  tenant: alpha
  namespaces: [tenant-alpha, tenant-beta]
zyntra:
  policy: |
    users:
      - {name: ops, roles: [approver]}
    service_tokens:
      - {name: ci, token_sha256: "00", roles: [viewer]}
YAML
render -f "$VALS" >"$OUT" || { echo "  FAIL render"; cat "$OUT"; exit 1; }
python3 - "$OUT" <<'EOF' || FAILED=1
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
sec = {(d["metadata"].get("namespace"), d["metadata"]["name"]): d for d in docs if d["kind"] == "Secret"}
tok = sec[("gryvia-system", "sovereign-aios-credentials")]["stringData"]["AGENT_ZYNTRA_TOKEN"]
pol = yaml.safe_load(sec[("gryvia-system", "sovereign-aios-zyntra-policy")]["stringData"]["policy.yaml"])
failed = 0
def ok(cond, msg):
    global failed
    print(("  ok   " if cond else "  FAIL ") + msg)
    failed |= not cond
ok(pol["users"] == [{"name": "ops", "roles": ["approver"]}], "zyntra.policy is kept")
names = [t["name"] for t in pol["service_tokens"]]
ok(names == ["ci", "gryvia-agents"], "the agent token is appended to zyntra.policy's service tokens")
ok(pol["service_tokens"][1]["roles"] == ["viewer"] and pol["service_tokens"][1]["tenant"] == "alpha", "roles and tenant")
for ns in ("tenant-alpha", "tenant-beta"):
    ok(sec[(ns, "zyntra-agent-token")]["stringData"]["token"] == tok, f"{ns} gets the token for its agents")
sys.exit(failed)
EOF
rm -f "$VALS"

echo "agent token off, bad token, bad policy"
render --set agentToken.enabled=false >"$OUT" || { echo "  FAIL render"; cat "$OUT"; exit 1; }
if grep -q "service_tokens" "$OUT"; then echo "  FAIL service token in the policy while off"; FAILED=1; else echo "  ok   no service token while off"; fi
if render --set agentToken.token=abc >/dev/null; then echo "  FAIL accepted a token without zst_"; FAILED=1; else echo "  ok   refuses a token without zst_"; fi
if render --set-string 'zyntra.policy=a: [' >/dev/null; then echo "  FAIL accepted invalid policy YAML"; FAILED=1; else echo "  ok   refuses invalid zyntra.policy"; fi

echo "netra and zyntra off"
render --set netra.enabled=false --set zyntra.enabled=false >"$OUT" || { echo "  FAIL render"; cat "$OUT"; exit 1; }
if grep -qE 'name: (netra|sovereign-aios-netra|sovereign-aios-zyntra-pack)$' "$OUT"; then
  echo "  FAIL Netra or Zyntra objects rendered while disabled"; FAILED=1
else
  echo "  ok   only Gryvia and the credentials"
fi
echo "hardening: OpenBao keys through External Secrets give the same Secrets"
A64="$(printf 'a%.0s' {1..64})" B64="$(printf 'b%.0s' {1..64})"
KV=(--set credentials.gryviaApiKey=apikey0123 --set "credentials.llmKey=gk-$A64"
    --set credentials.netraApiKey=netra0123 --set credentials.netraAgentKey=agent0123
    --set "agentToken.token=zst_$B64" --set 'agentToken.namespaces={tenant-alpha}')
PLAIN="$(mktemp)"
render "${KV[@]}" >"$PLAIN" || { echo "  FAIL render"; cat "$PLAIN"; exit 1; }
render "${KV[@]}" --set hardening.openbao.enabled=true >"$OUT" || { echo "  FAIL render"; cat "$OUT"; exit 1; }
python3 - "$PLAIN" "$OUT" <<'EOF' || FAILED=1
import hashlib, re, sys, yaml
plain = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
bao = [d for d in yaml.safe_load_all(open(sys.argv[2])) if d]
kv = {"gryviaApiKey": "apikey0123", "llmKey": "gk-" + "a" * 64, "netraApiKey": "netra0123",
      "netraAgentKey": "agent0123", "agentZyntraToken": "zst_" + "b" * 64}
failed = 0
def ok(cond, msg):
    global failed
    print(("  ok   " if cond else "  FAIL ") + msg)
    failed |= not cond
def eso(text):  # the two forms the chart emits: {{ .field }} and {{ .field | sha256sum }}
    def sub(m):
        v = kv[m.group(1)]
        return hashlib.sha256(v.encode()).hexdigest() if m.group(2) else v
    return re.sub(r"\{\{ \.(\w+)( \| sha256sum)? \}\}", sub, text)
mine = lambda d: d["metadata"].get("labels", {}).get("app.kubernetes.io/name") == "sovereign-aios"
want = {(d["metadata"]["namespace"], d["metadata"]["name"]): d for d in plain if d["kind"] == "Secret" and mine(d)}
got = {(d["metadata"]["namespace"], d["metadata"]["name"]): d for d in bao if d["kind"] == "ExternalSecret"}
ok(set(got) == set(want) and len(want) == 6, f"every chart Secret becomes an ExternalSecret ({len(want)})")
ok(not [d for d in bao if d["kind"] == "Secret" and mine(d)], "no generated key is rendered")
for k, w in want.items():
    g = got.get(k)
    if not g:
        continue
    t = g["spec"]["target"]
    norm = lambda d: {f: yaml.safe_load(v) if f.endswith(".yaml") else v for f, v in d.items()}
    data = {f: eso(v) for f, v in t["template"]["data"].items()}
    ok(norm(data) == norm(w["stringData"]), f"{k[0]}/{k[1]}: External Secrets writes the same data")
    ok(t["template"]["metadata"]["labels"] == w["metadata"]["labels"], f"{k[0]}/{k[1]}: and the same labels")
    ok(g["spec"]["dataFrom"] == [{"extract": {"key": "sovereign-aios"}}] and
       g["spec"]["secretStoreRef"] == {"kind": "ClusterSecretStore", "name": "sovereign-aios-openbao"}, f"{k[0]}/{k[1]}: from OpenBao")
st = [d for d in bao if d["kind"] == "ClusterSecretStore"][0]["spec"]
ok(st["provider"]["vault"]["version"] == "v2" and st["provider"]["vault"]["auth"]["kubernetes"]["role"] == "sovereign-aios",
   "the store reads KV v2 with OpenBao's Kubernetes auth")
ok(set(st["conditions"][0]["namespaces"]) == {"gryvia-system", "gryvia-llm-keys", "tenant-alpha"}, "only the chart's namespaces may use it")
sys.exit(failed)
EOF
rm -f "$PLAIN"

echo "hardening: Kyverno, Falco and SPIRE"
render --set hardening.kyverno.enabled=true --set hardening.falco.enabled=true --set hardening.spire.enabled=true \
  --set 'hardening.spire.agentNamespaces={tenant-alpha}' >"$OUT" || { echo "  FAIL render"; cat "$OUT"; exit 1; }
python3 - "$OUT" <<'EOF' || FAILED=1
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
by = {(d["kind"], d["metadata"]["name"]): d for d in docs}
failed = 0
def ok(cond, msg):
    global failed
    print(("  ok   " if cond else "  FAIL ") + msg)
    failed |= not cond
reg = by[("ClusterPolicy", "sovereign-aios-allowed-registries")]["spec"]["rules"][0]
ok(reg["validate"]["failureAction"] == "Audit", "policies audit by default")
ok(reg["validate"]["pattern"]["spec"]["containers"][0]["image"].startswith("ghcr.io/zyvorai/* | nvcr.io/nvidia/*"), "registry allowlist")
ok({"kube-system", "kyverno", "falco"} <= set(reg["exclude"]["any"][0]["resources"]["namespaces"]), "kube-system and the add-ons are exempt")
vi = by[("ClusterPolicy", "sovereign-aios-verify-images")]["spec"]["rules"][0]["verifyImages"][0]
subs = [e["keyless"]["subject"] for e in vi["attestors"][0]["entries"]]
ok(vi["imageReferences"] == ["ghcr.io/zyvorai/*"] and len(subs) == 3 and all("/.github/workflows/" in s for s in subs),
   "Zyvor images need a signature from one of the three release workflows")
ok(vi["attestors"][0]["count"] == 1, "any one of them")
images = [c["image"] for d in docs if d["kind"] in ("Deployment", "DaemonSet", "StatefulSet")
          for c in d["spec"]["template"]["spec"].get("containers", []) + d["spec"]["template"]["spec"].get("initContainers", [])]
import fnmatch
alts = [a.strip() for a in reg["validate"]["pattern"]["spec"]["containers"][0]["image"].split("|")]
bad = [i for i in images if not any(fnmatch.fnmatch(i, a) for a in alts)]
ok(images and not bad, f"the chart's own {len(images)} containers pass the allowlist {bad or ''}")
fr = by[("ConfigMap", "sovereign-aios-falco-rules")]
ok(fr["metadata"]["namespace"] == "falco" and "GryviaAgent runtime started a program" in fr["data"]["sovereign-aios-rules.yaml"], "Falco rules ConfigMap")
ag = by[("ClusterSPIFFEID", "sovereign-aios-agents")]["spec"]
ok(ag["spiffeIDTemplate"].startswith("spiffe://sovereign.local/agent/") and ag["podSelector"] == {"matchLabels": {"gryvia.io/component": "agent"}}, "agents get their own SPIFFE IDs")
ok(ag["namespaceSelector"]["matchExpressions"][0]["values"] == ["gryvia-system", "tenant-alpha"], "in the agent namespaces")
pl = by[("ClusterSPIFFEID", "sovereign-aios-platform")]["spec"]
ok("/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}" in pl["spiffeIDTemplate"], "platform pods by service account")
sys.exit(failed)
EOF
if render --set hardening.kyverno.enabled=true --set hardening.kyverno.verifyImages.mode=key >/dev/null; then
  echo "  FAIL key mode without a public key"; FAILED=1
else
  echo "  ok   key mode needs a public key"
fi
rm -f "$OUT"

[[ $FAILED -eq 0 ]] && echo "PASS" || { echo "FAIL"; exit 1; }
