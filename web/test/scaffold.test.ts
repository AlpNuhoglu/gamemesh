import { expect, test } from "vitest";
import vectors from "../../internal/ghosttag/testdata/validate_vectors.json";

// The golden vectors of the movement validation are read from where the Go
// tests keep them. This only proves the file is reachable from here; the
// test that runs them through the client's own formula replaces it.
test("the golden vector file is readable from the client's tests", () => {
  expect(vectors.cases.length).toBeGreaterThan(0);
  expect(vectors.cfg.v_runner).toBeGreaterThan(0);
});
