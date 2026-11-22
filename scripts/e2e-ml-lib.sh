#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Helpers for .github/workflows/e2e-ml.yml (source this file). Not meant to be run on its own.
#
# The ML controllers (workspace, inference, model registry, workflow, auto tuner, model watch) are exercised on a kind
# cluster with tiny CPU images, so no GPU is involved. What is NOT proven by this e2e: real Jupyter/code-server/vLLM/
# Triton images, GPUs, an HPA reacting to real metrics (kind has no metrics-server here), the real Hugging Face API and
# a real download/fine-tune/evaluation (the model factory steps are busybox stand-ins that only emit outputs).

NS="${E2E_NS:-gryvia-system}"
API="${E2E_API:-https://localhost:8443}"
KEY="${E2E_KEY:-Admin@321}"

api() { # api PATH [curl args...]: authenticated gateway call, fails on HTTP errors
  local path="$1"; shift
  curl -sfk -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' "$@" "$API$path"
}

api_status() { # api_status PATH [curl args...]: HTTP status code only
  local path="$1"; shift
  curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' "$@" "$API$path"
}

wait_for() { # wait_for "what" SECONDS command...: poll until the command succeeds
  local what="$1" timeout="$2"; shift 2
  local end=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$end" ]; then echo "TIMEOUT after ${timeout}s waiting for: $what" >&2; return 1; fi
    sleep 3
  done
  echo "ok: $what"
}

jp() { # jp KIND/NAME JSONPATH: a field of an object in the namespace
  kubectl -n "$NS" get "$1" -o "jsonpath=$2" 2>/dev/null
}

field_is() { [ "$(jp "$1" "$2")" = "$3" ]; }        # field_is KIND/NAME JSONPATH VALUE
field_set() { [ -n "$(jp "$1" "$2")" ]; }            # field_set KIND/NAME JSONPATH
field_empty() { [ -z "$(jp "$1" "$2")" ]; }          # field_empty KIND/NAME JSONPATH
gone() { ! kubectl -n "$NS" get "$@" >/dev/null 2>&1; } # gone KIND/NAME

assert_eq() { # assert_eq "what" ACTUAL EXPECTED
  if [ "$2" != "$3" ]; then echo "ASSERT FAILED: $1: got '$2', want '$3'" >&2; return 1; fi
  echo "ok: $1 = $3"
}

assert_nonempty() { # assert_nonempty "what" VALUE
  if [ -z "$2" ] || [ "$2" = "null" ]; then echo "ASSERT FAILED: $1 is empty" >&2; return 1; fi
  echo "ok: $1 = $2"
}

gw() { api "$1" | jq -r "$2"; } # gw PATH JQ_FILTER: a field of a gateway answer

# --- Model factory: a stand-in Hugging Face hub (deploy/e2e-fake-hf) serving /tmp/models.json at /api/models.

hf_model() { # hf_model NAME N [LICENSE] [PARAMS]: one hub entry for e2e-org/NAME with revision N (as 40 hex digits)
  jq -nc --arg id "e2e-org/$1" --arg sha "$(printf '%040d' "$2")" --arg lic "${3:-apache-2.0}" --argjson p "${4:-135000000}" \
    '{id: $id, sha: $sha, lastModified: "2026-01-01T00:00:00Z", downloads: 5000, gated: false,
      pipeline_tag: "text-generation", tags: ["text-generation", ("license:" + $lic)], safetensors: {total: $p}}'
}

hub_models() { # hub_models ENTRY...: replace what the fake hub lists
  printf '%s\n' "$@" | jq -sc . | kubectl -n "$NS" exec -i deploy/e2e-fake-hf -- \
    sh -c 'cat > /tmp/models.json.new && mv /tmp/models.json.new /tmp/models.json'
}

run_field() { # run_field WATCH MODEL_ID FIELD: a field of one candidate in the watch status
  jp "gryviamodelwatch/$1" "{.status.candidates[?(@.id==\"$2\")].$3}"
}
run_is() { [ "$(run_field "$1" "$2" "$3")" = "$4" ]; } # run_is WATCH MODEL_ID FIELD VALUE

entry_of() { # entry_of WORKFLOW: the GryviaModelRegistry entry a workflow's register step created
  kubectl -n "$NS" get gryviamodelregistries -o "jsonpath={.items[?(@.spec.source.workflowRef==\"$1\")].metadata.name}"
}
