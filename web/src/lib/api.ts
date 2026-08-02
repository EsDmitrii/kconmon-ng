import type { Matrix, Problem, PromResult, Protocol, Topology, Version } from "./types";

export class ApiError extends Error {
  constructor(public problem: Problem) {
    super(problem.detail ?? problem.title);
    this.name = "ApiError";
  }
}

async function handle<T>(resp: Response): Promise<T> {
  if (resp.ok) return (await resp.json()) as T;
  const ct = resp.headers.get("Content-Type") ?? "";
  if (ct.includes("problem+json")) throw new ApiError((await resp.json()) as Problem);
  // PromQL upstream errors come back as Prometheus's own envelope with 4xx.
  if (ct.includes("json")) {
    const body = (await resp.json()) as PromResult;
    if (body.status === "error") return body as T;
  }
  throw new ApiError({ type: "about:blank", title: resp.statusText, status: resp.status });
}

export function getTopology(): Promise<Topology> {
  return fetch("/api/v1/topology").then((r) => handle<Topology>(r));
}

export function getVersion(): Promise<Version> {
  return fetch("/api/v1/version").then((r) => handle<Version>(r));
}

export function getMatrix(protocol: Protocol, plane = "pod"): Promise<Matrix> {
  const qs = new URLSearchParams({ protocol, plane });
  return fetch(`/api/v1/matrix?${qs}`).then((r) => handle<Matrix>(r));
}

export function promqlQuery(query: string, time?: Date): Promise<PromResult> {
  return fetch("/api/v1/promql/query", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, ...(time ? { time: time.toISOString() } : {}) }),
  }).then((r) => handle<PromResult>(r));
}

export function promqlQueryRange(query: string, start: Date, end: Date, stepNs: number): Promise<PromResult> {
  return fetch("/api/v1/promql/query_range", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, start: start.toISOString(), end: end.toISOString(), step: stepNs }),
  }).then((r) => handle<PromResult>(r));
}
