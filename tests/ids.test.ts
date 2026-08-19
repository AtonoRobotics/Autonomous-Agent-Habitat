import { describe, expect, it } from "vitest";
import { isId, newId } from "../src/ids.js";

describe("ids", () => {
  it("mints a prefixed id with 24 hex characters", () => {
    expect(newId("run")).toMatch(/^run_[0-9a-f]{24}$/);
  });

  it("does not repeat", () => {
    const minted = new Set(Array.from({ length: 500 }, () => newId("card")));
    expect(minted.size).toBe(500);
  });

  it("recognises its own ids and rejects a foreign shape", () => {
    expect(isId(newId("grant"), "grant")).toBe(true);
    expect(isId(newId("grant"), "card")).toBe(false);
    expect(isId("grant_notlongenough")).toBe(false);
    expect(isId("nosuchkind_0123456789abcdef01234567")).toBe(false);
  });
});
