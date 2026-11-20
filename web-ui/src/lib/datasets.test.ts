// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from "vitest";
import { mountLabel, sourceLabel, versionsNewestFirst } from "./datasets";

describe("datasets", () => {
  it("labels the source", () => {
    expect(sourceLabel({ spec: { source: { type: "s3", location: "s3://b/p" } } })).toBe("s3 s3://b/p");
    expect(sourceLabel({ spec: { source: { type: "nfs" } } })).toBe("nfs");
    expect(sourceLabel({ spec: {} })).toBe("—");
  });

  it("labels the mount", () => {
    expect(mountLabel({ status: { namespace: "ml", pvcName: "dataset-c", subPath: "v1" } })).toBe("ml/dataset-c:v1");
    expect(mountLabel({ status: { pvcName: "dataset-c" } })).toBe("dataset-c");
    expect(mountLabel({ status: {} })).toBe("—");
  });

  it("orders versions newest first without mutating", () => {
    const d = { status: { versions: [{ version: "v1" }, { version: "v2" }] } };
    expect(versionsNewestFirst(d).map((v) => v.version)).toEqual(["v2", "v1"]);
    expect(d.status.versions[0].version).toBe("v1");
  });
});
