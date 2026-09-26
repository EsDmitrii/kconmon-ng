import { renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { stubViewport } from "@/lib/viewport-stub";
import { useMediaQuery } from "./use-media-query";

const PHONE = "not all and (min-width: 48rem)";

afterEach(() => vi.unstubAllGlobals());

describe("useMediaQuery", () => {
  it("answers on the first render, so a layout built from it needs no second pass", () => {
    stubViewport(390);
    const { result } = renderHook(() => useMediaQuery(PHONE));
    expect(result.current).toBe(true);
  });

  it("follows the viewport across the breakpoint", () => {
    const viewport = stubViewport(1440);
    const { result } = renderHook(() => useMediaQuery(PHONE));
    expect(result.current).toBe(false);
    viewport.resize(390);
    expect(result.current).toBe(true);
    viewport.resize(1024);
    expect(result.current).toBe(false);
  });

  it("answers false where there is no matchMedia at all", () => {
    vi.stubGlobal("matchMedia", undefined);
    const { result } = renderHook(() => useMediaQuery(PHONE));
    expect(result.current).toBe(false);
  });
});
