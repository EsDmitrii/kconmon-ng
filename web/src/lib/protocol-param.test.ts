import { afterEach, describe, expect, it } from "vitest";
import { matrixHref, writeProtocol } from "./protocol-param";

afterEach(() => window.history.replaceState(null, "", "/"));

describe("writeProtocol", () => {
  /* The router keeps its history index in history.state; replacing it with {} makes the next push
     store a NaN index. */
  it("keeps the router's history state while it rewrites ?protocol=", () => {
    window.history.replaceState({ __TSR_index: 3, key: "k1" }, "", "/matrix?at=x");
    writeProtocol("udp");
    expect(window.location.search).toContain("protocol=udp");
    expect(window.history.state).toEqual({ __TSR_index: 3, key: "k1" });
  });
});

describe("matrixHref", () => {
  it("leaves the default protocol unspelled and names every other one in the Matrix's own key", () => {
    expect(matrixHref("tcp")).toBe("/matrix");
    expect(matrixHref("udp")).toBe("/matrix?protocol=udp");
    expect(matrixHref("pmtu")).toBe("/matrix?protocol=pmtu");
  });
});
