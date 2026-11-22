// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

export interface VectorIndex {
  metadata: { name: string; namespace?: string; creationTimestamp?: string };
  spec: {
    datasetRef?: string;
    embeddingModel?: string;
    chunking?: { size?: number; overlap?: number };
    store?: "managed" | "external";
    storeURL?: string;
    collection?: string;
    schedule?: string;
    suspend?: boolean;
  };
  status: {
    phase?: string;
    message?: string;
    storeURL?: string;
    collection?: string;
    documents?: number;
    chunks?: number;
    dimensions?: number;
    datasetVersion?: string;
    lastIngested?: string;
    ingestJob?: string;
    nextScheduleTime?: string;
    keySecret?: string;
  };
}

export interface VectorIndexList {
  items: VectorIndex[];
  retrieveURL: string;
}

/** "managed Qdrant" or "external https://…". */
export function storeLabel(idx: Pick<VectorIndex, "spec">): string {
  if (idx.spec.store === "external") return idx.spec.storeURL ? `external ${idx.spec.storeURL}` : "external";
  return "managed Qdrant";
}

/** "12 chunks · 3 docs", or "—" before the first ingestion. */
export function contentLabel(idx: Pick<VectorIndex, "status">): string {
  const { chunks, documents } = idx.status;
  if (!chunks && !documents) return "—";
  return `${chunks ?? 0} chunks · ${documents ?? 0} docs`;
}

/** Shown in place of the phase while the index is suspended. */
export function displayPhase(idx: Pick<VectorIndex, "spec" | "status">): string {
  if (idx.spec.suspend) return "Suspended";
  return idx.status.phase ?? "Pending";
}

/** Whether a reingest makes sense: not suspended and not already ingesting. */
export function canReingest(idx: Pick<VectorIndex, "spec" | "status">): boolean {
  return !idx.spec.suspend && idx.status.phase !== "Ingesting";
}

/** A curl call of the gateway's /v1/retrieve for this index. */
export function retrieveCurl(retrieveURL: string, index: string): string {
  const url = retrieveURL || "http://<llm-gateway>:8080/v1/retrieve";
  const body = JSON.stringify({ index, query: "your question", topK: 4 });
  return `curl -s ${url} \\\n  -H "Authorization: Bearer $GRYVIA_LLM_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '${body}'`;
}
