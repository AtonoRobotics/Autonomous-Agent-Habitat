import { describe, expect, it } from "vitest";
import { isRefusal, Refusal, REFUSAL_CODES } from "../src/refusals.js";

describe("refusals", () => {
  it("carries a code callers can match on", () => {
    const refusal = new Refusal("SURFACE_VIOLATION", "A field token cannot sit in the habitat.");
    expect(isRefusal(refusal, "SURFACE_VIOLATION")).toBe(true);
    expect(isRefusal(refusal, "ONE_GOAL")).toBe(false);
    expect(isRefusal(new Error("plain"))).toBe(false);
  });

  it("names no trust tier", () => {
    for (const code of REFUSAL_CODES) {
      expect(code).not.toMatch(/TRUST|TIER|T[0-3]\b/);
    }
  });
});
