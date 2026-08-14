import { expect, test } from "bun:test";
import isNumber from "is-number";

test("installed dependency runs", () => {
  expect(isNumber("42")).toBe(true);
});
