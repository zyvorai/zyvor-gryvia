// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

export interface DatasetVersion {
  version: string;
  createdAt?: string;
  size?: number;
  checksum?: string;
}

export interface Dataset {
  metadata: { name: string; creationTimestamp?: string };
  spec: {
    type?: string;
    description?: string;
    license?: string;
    version?: string;
    namespace?: string;
    source?: { type?: string; location?: string };
  };
  status: {
    state?: string;
    message?: string;
    namespace?: string;
    pvcName?: string;
    subPath?: string;
    currentVersion?: string;
    fileCount?: number;
    totalSizeBytes?: number;
    versions?: DatasetVersion[];
  };
}

/** "http https://…", or "—" when the source is missing. */
export function sourceLabel(d: Pick<Dataset, "spec">): string {
  const s = d.spec.source;
  if (!s?.type) return "—";
  return s.location ? `${s.type} ${s.location}` : s.type;
}

/** Where a job mounts the current version: "ns/pvc:subPath". */
export function mountLabel(d: Pick<Dataset, "status">): string {
  const { namespace, pvcName, subPath } = d.status;
  if (!pvcName) return "—";
  return `${namespace ? `${namespace}/` : ""}${pvcName}${subPath ? `:${subPath}` : ""}`;
}

/** Versions newest first. */
export function versionsNewestFirst(d: Pick<Dataset, "status">): DatasetVersion[] {
  return [...(d.status.versions ?? [])].reverse();
}
