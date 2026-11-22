#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Offline checks for the Sovereign AI OS air gap (deploy/airgap, deploy/argocd/sovereign-aios): the chart's images are
# in deploy/airgap/images.txt; with values-airgap.yaml every image comes from the Harbor mirror under the path
# scripts/airgap-mirror.sh pushes it to and nothing points outside the cluster; the Zarf package carries exactly
# images.txt; Zarf and Argo CD pin the chart versions in charts.yaml. No network: the add-on images are checked by
# `scripts/sovereign-aios-images.sh --check` in CI. Needs helm and python3 with PyYAML.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
[[ -d helm/sovereign-aios/charts ]] || scripts/sovereign-aios-deps.sh >/dev/null || { echo "sovereign-aios-deps failed"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
render() { helm template sa helm/sovereign-aios -n gryvia-system --kube-version 1.31.0 "$@"; }
render >"$WORK/default.yaml" || { echo "FAIL render"; exit 1; }
render -f deploy/airgap/values-airgap.yaml --set hardening.kyverno.verifyImages.publicKey=test >"$WORK/airgap.yaml" \
  || { echo "FAIL render with values-airgap.yaml"; exit 1; }

# The mirror script's own path mapping, so the test and the script cannot drift apart.
eval "$(sed -n '/^mirror_path()/,/^}/p' scripts/airgap-mirror.sh)"
while read -r img; do printf '%s %s\n' "$img" "$(mirror_path "$img")"; done <deploy/airgap/images.txt >"$WORK/paths.txt"
printf '%s\n' busybox:1 alpine/openssl:3 ghcr.io/zyvorai/x:1 localhost:5000/a/b:1 \
  | while read -r i; do printf '%s %s\n' "$i" "$(mirror_path "$i")"; done >"$WORK/samples.txt"

python3 - "$WORK" <<'EOF'
import os, re, sys, yaml
w = sys.argv[1]
failed = 0
def ok(cond, msg):
    global failed
    print(("  ok   " if cond else "  FAIL ") + msg)
    failed |= not cond
load_all = lambda p: [d for d in yaml.safe_load_all(open(p)) if d]
load = lambda p: yaml.safe_load(open(p))

def images(docs):
    out = set()
    for d in docs:
        if d["kind"] not in ("Deployment", "DaemonSet", "StatefulSet", "Job", "CronJob", "Pod"):
            continue
        spec = d["spec"]["template"]["spec"] if d["kind"] != "Pod" else d["spec"]
        for c in (spec.get("initContainers") or []) + (spec.get("containers") or []):
            out.add(c["image"])
            out.update(a.split("=", 1)[1] for a in c.get("args") or [] if isinstance(a, str) and a.startswith("--agent-image="))
    return out

def hub(ref):
    first = ref.split("/")[0]
    if "/" not in ref:
        return "docker.io/library/" + ref
    return ref if ("." in first or ":" in first or first == "localhost") else "docker.io/" + ref

listed = [l.strip() for l in open("deploy/airgap/images.txt") if l.strip()]
paths = dict(l.split() for l in open(os.path.join(w, "paths.txt")))
print("image list")
ok(listed == sorted(set(listed)), "images.txt is sorted without duplicates")
mine = {hub(i) for i in images(load_all(os.path.join(w, "default.yaml")))}
ok(mine <= set(listed), f"the chart's {len(mine)} images are listed {sorted(mine - set(listed)) or ''}")
ok(not [i for i in mine if i.endswith(":latest")], "no chart image uses latest")
samples = dict(l.split() for l in open(os.path.join(w, "samples.txt")))
ok(samples == {"busybox:1": "library/busybox:1", "alpine/openssl:3": "alpine/openssl:3",
               "ghcr.io/zyvorai/x:1": "zyvorai/x:1", "localhost:5000/a/b:1": "a/b:1"}, "mirror paths drop the registry host")
ok(len(set(paths.values())) == len(paths), "no two images share a mirror path")

print("values-airgap.yaml")
ag = load_all(os.path.join(w, "airgap.yaml"))
H = "harbor.sovereign.internal/"
used = images(ag)
ok(used and all(i.startswith(H) for i in used), f"all {len(used)} images come from the mirror {sorted(i for i in used if not i.startswith(H)) or ''}")
want = {H + paths[i] for i in mine}
ok(used == want, "each is the mirror path of a listed image " + str(sorted(used ^ want) or ""))
hosts = set()
for d in ag:
    hosts.update(re.findall(r"https?://([A-Za-z0-9.\-]+)", yaml.safe_dump(d)))
local = lambda h: "." not in h or h.endswith((".svc", ".svc.cluster.local", ".cluster.local"))
ok(all(local(h) for h in hosts), f"every URL is inside the cluster {sorted(h for h in hosts if not local(h)) or ''}")
pols = {d["metadata"]["name"]: d for d in ag if d["kind"] == "ClusterPolicy"}
vi = pols["sovereign-aios-verify-images"]["spec"]["rules"][0]["verifyImages"][0]
ok(vi["imageReferences"] == [H + "zyvorai/*"] and "keys" in vi["attestors"][0]["entries"][0], "Kyverno checks the mirrored images with a key")
ok(vi["failureAction"] == "Enforce", "and enforces it")
reg = pols["sovereign-aios-allowed-registries"]["spec"]["rules"][0]["validate"]["pattern"]["spec"]["containers"][0]["image"]
ok(reg == H + "*", "only the mirror is an allowed registry")

print("Zarf package")
charts = {c["name"]: c for c in load("deploy/airgap/charts.yaml")["charts"]}
chart_version = load("helm/sovereign-aios/Chart.yaml")["version"]
z = load("deploy/airgap/zarf.yaml")
zimgs = [i for c in z["components"] for i in c.get("images", [])]
ok(sorted(zimgs) == listed, "the components carry exactly images.txt")
zch = {c["name"]: c for comp in z["components"] for c in comp.get("charts", [])}
ok(all(zch[n]["version"] == c["version"] and zch[n]["url"] == c["repo"] for n, c in charts.items()), "add-on charts at the pinned versions")
sa = zch["sovereign-aios"]
ok(sa["version"] == chart_version and os.path.isdir(os.path.join("deploy/airgap", sa["localPath"])), "the platform chart from this repository")
ok([c["name"] for c in z["components"] if c.get("required")] == ["sovereign-aios"], "only the platform is required")

print("Argo CD app of apps")
apps = {}
for f in sorted(os.listdir("deploy/argocd/sovereign-aios/apps")):
    a = load(os.path.join("deploy/argocd/sovereign-aios/apps", f))
    apps[a["metadata"]["name"]] = a
ok(set(apps) == set(charts) | {"sovereign-aios"}, "one Application per chart")
ok(all(apps[n]["spec"]["source"]["targetRevision"] == c["version"] for n, c in charts.items()), "add-ons at the pinned versions")
ok(apps["sovereign-aios"]["spec"]["source"]["targetRevision"] == chart_version, "the platform at the chart's version")
ok(apps["sovereign-aios"]["spec"]["source"]["helm"]["valuesObject"] == load("deploy/airgap/values-airgap.yaml"), "the platform with values-airgap.yaml")
ok(apps["falco"]["spec"]["source"]["helm"]["valuesObject"] == load("deploy/hardening/falco-values.yaml"), "Falco with falco-values.yaml")
td = apps["spire"]["spec"]["source"]["helm"]["valuesObject"]["global"]["spire"]["trustDomain"]
ok(td == load("helm/sovereign-aios/values.yaml")["hardening"]["spire"]["trustDomain"], "SPIRE's trust domain matches the chart's")
waves = {n: int(a["metadata"]["annotations"]["argocd.argoproj.io/sync-wave"]) for n, a in apps.items()}
ok(all(waves["sovereign-aios"] > v for n, v in waves.items() if n != "sovereign-aios") and waves["spire-crds"] < waves["spire"],
   "add-ons sync before the platform, CRDs before SPIRE")
root = load_all("deploy/argocd/sovereign-aios/root.yaml")
proj = [d for d in root if d["kind"] == "AppProject"][0]["spec"]
ok(all(a["spec"]["source"]["repoURL"] in proj["sourceRepos"] and a["spec"]["project"] == "sovereign-aios" for a in apps.values()),
   "every source is in the project's allowed repositories")
dests = {d["namespace"] for d in proj["destinations"]}
ok(all(a["spec"]["destination"]["namespace"] in dests for a in apps.values()), "and every destination namespace")
ok(all(a["spec"]["source"]["repoURL"] == H + "charts" for a in apps.values()), "charts come from the mirror")
sys.exit(failed)
EOF
rc=$?
[[ $rc -eq 0 ]] && echo PASS || { echo FAIL; exit 1; }
