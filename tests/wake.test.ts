import { describe, expect, it } from "vitest";
import { cutsInLine, isWakeKind, WAKE_KINDS } from "../src/wake.js";

describe("wake kinds", () => {
  it("is a closed set of twelve", () => {
    expect(WAKE_KINDS).toHaveLength(12);
    expect(new Set(WAKE_KINDS).size).toBe(12);
  });

  it("admits nothing outside the set", () => {
    expect(isWakeKind("field_start")).toBe(true);
    expect(isWakeKind("chat")).toBe(false);
    expect(isWakeKind("")).toBe(false);
  });

  it("lets only a kill cut the queue", () => {
    const cutting = WAKE_KINDS.filter(cutsInLine);
    expect(cutting).toEqual(["kill"]);
  });
});
