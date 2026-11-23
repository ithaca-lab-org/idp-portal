import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import { App } from "./App";

type Reply = { status?: number; body?: unknown };
type Route = (init?: RequestInit) => Reply;

function mockFetch(routes: Record<string, Route>) {
  const f = vi.fn(async (path: string, init?: RequestInit) => {
    const r = routes[`${init?.method ?? "GET"} ${path}`];
    if (!r) throw new Error(`unexpected ${init?.method ?? "GET"} ${path}`);
    const { status = 200, body } = r(init);
    return new Response(JSON.stringify(body), { status });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

const SHA = "0123456789abcdef0123456789abcdef01234567";
const user = { sub: "u1", username: "user03", email: "u@x.com", groups: ["payment-bu"] };
const pipe = (o: object = {}) => ({
  namespace: "team-a", name: "orders", workload: "orders", repo: "https://github.com/ithaca-lab-org/orders",
  path: "deploy/base", phase: "READY", applicationName: "team-a-orders", ...o,
});
const run = (o: object = {}) => ({
  namespace: "team-a", name: "orders-0123abcd", pipeline: "orders", ref: SHA, requestedBy: "u1",
  phase: "SUCCEEDED", message: "deployed", executionId: "01HX", createdAt: "2026-11-22T10:00:00Z", ...o,
});

function base(extra: Record<string, Route> = {}, state: { pipes?: object[]; runs?: object[]; workloads?: object[] } = {}) {
  return mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
    "GET /api/pipelines/namespaces": () => ({
      body: { namespaces: [{ name: "team-a", businessUnit: "payment-bu" }], allowedOrgs: ["ithaca-lab-org"], defaultPath: "deploy/base", maxRuns: 100 },
    }),
    "GET /api/namespaces/team-a/pipelines": () => ({ body: { pipelines: state.pipes ?? [] } }),
    "GET /api/namespaces/team-a/pipelines/orders/runs": () => ({ body: { runs: state.runs ?? [] } }),
    "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: state.workloads ?? [{ name: "orders" }, { name: "billing" }] } }),
    ...extra,
  });
}

async function openPipelines() {
  render(<App />);
  await userEvent.click(await screen.findByRole("button", { name: "Pipelines" }));
}

const posts = (f: ReturnType<typeof mockFetch>) => f.mock.calls.filter(([, init]) => init?.method === "POST");

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

test("the Pipelines page is hidden when the server has it turned off", async () => {
  base({ "GET /api/pipelines/namespaces": () => ({ status: 404, body: { error: "not found" } }) });
  render(<App />);
  expect(await screen.findByText("No orders yet.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Pipelines" })).not.toBeInTheDocument();
});

test("lists pipelines with phase badge and message, and the run history", async () => {
  base({}, { pipes: [pipe({ phase: "FAILED", message: "The repository could not be reached." })], runs: [run()] });
  await openPipelines();
  const card = await screen.findByRole("region", { name: "Pipeline orders" });
  expect(within(card).getByText("FAILED")).toHaveClass("badge-failed");
  expect(within(card).getByText("The repository could not be reached.")).toBeInTheDocument();
  expect(within(card).getByText("team-a-orders")).toBeInTheDocument();
  const row = (await within(card).findByText("0123456")).closest("tr")!;
  expect(within(row).getByText("SUCCEEDED")).toBeInTheDocument();
  expect(within(row).getByText("u1")).toBeInTheDocument();
  expect(within(row).getByText("deployed")).toBeInTheDocument();
  // not READY: no deploy yet
  expect(within(card).getByRole("button", { name: "Deploy orders" })).toBeDisabled();
});

test("ordering a pipeline checks the repo and path first, then posts workload, repo and path", async () => {
  let pipes: object[] = [];
  const f = base(
    {
      "POST /api/namespaces/team-a/pipelines": () => {
        pipes = [pipe({ phase: "PENDING", applicationName: "" })];
        return { status: 202, body: pipes[0] };
      },
      "GET /api/namespaces/team-a/pipelines": () => ({ body: { pipelines: pipes } }),
    },
    {},
  );
  await openPipelines();
  const repo = await screen.findByLabelText("Repository");
  await screen.findByRole("option", { name: "billing" });
  await userEvent.type(repo, "https://github.com/evil/orders");
  await userEvent.click(screen.getByRole("button", { name: "Order pipeline" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("not allowed");
  await userEvent.clear(repo);
  await userEvent.type(repo, "https://github.com/ithaca-lab-org/orders");
  await userEvent.type(screen.getByLabelText("Path (optional)"), "a/../b");
  await userEvent.click(screen.getByRole("button", { name: "Order pipeline" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("without '..'");
  expect(posts(f)).toHaveLength(0);

  await userEvent.clear(screen.getByLabelText("Path (optional)"));
  await userEvent.selectOptions(screen.getByLabelText("Workload"), "orders");
  await userEvent.click(screen.getByRole("button", { name: "Order pipeline" }));
  expect(await screen.findByText(/Ordered the pipeline for orders/)).toBeInTheDocument();
  expect(JSON.parse(posts(f)[0][1]!.body as string)).toEqual({ workload: "orders", repo: "https://github.com/ithaca-lab-org/orders" });
  expect(await screen.findByRole("region", { name: "Pipeline orders" })).toBeInTheDocument();
  // a workload that has a pipeline is no longer offered
  expect(screen.queryByRole("option", { name: "orders" })).not.toBeInTheDocument();
});

test("shows the API's error for a duplicate pipeline", async () => {
  base({ "POST /api/namespaces/team-a/pipelines": () => ({ status: 409, body: { error: "a pipeline for this workload already exists in that namespace" } }) });
  await openPipelines();
  await screen.findByRole("option", { name: "orders" });
  await userEvent.type(screen.getByLabelText("Repository"), "https://github.com/ithaca-lab-org/orders");
  await userEvent.click(screen.getByRole("button", { name: "Order pipeline" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("already exists");
});

test("deploying posts only the ref; a tag or short SHA is refused before the request", async () => {
  let runs: object[] = [];
  const f = base(
    {
      "POST /api/namespaces/team-a/pipelines/orders/runs": () => {
        runs = [run({ phase: "PENDING", message: "", executionId: "" })];
        return { status: 202, body: runs[0] };
      },
      "GET /api/namespaces/team-a/pipelines/orders/runs": () => ({ body: { runs } }),
    },
    { pipes: [pipe()] },
  );
  await openPipelines();
  const input = await screen.findByLabelText("Commit SHA for orders");
  const button = screen.getByRole("button", { name: "Deploy orders" });
  await userEvent.type(input, "v1.2.3");
  await userEvent.click(button);
  expect(await screen.findByRole("alert")).toHaveTextContent("Deploy by commit");
  await userEvent.clear(input);
  await userEvent.type(input, "0123456");
  await userEvent.click(button);
  expect(await screen.findByRole("alert")).toHaveTextContent("40-character");
  expect(posts(f)).toHaveLength(0);

  await userEvent.clear(input);
  await userEvent.type(input, SHA);
  await userEvent.click(button);
  expect(await screen.findByText("Deploy of 0123456 requested.")).toBeInTheDocument();
  expect(JSON.parse(posts(f)[0][1]!.body as string)).toEqual({ ref: SHA });
  expect(await screen.findByText("PENDING")).toBeInTheDocument();
});

test("shows the run cap message from the API", async () => {
  base({ "POST /api/namespaces/team-a/pipelines/orders/runs": () => ({ status: 429, body: { error: "this namespace has reached its limit of 100 delivery runs" } }) }, { pipes: [pipe()] });
  await openPipelines();
  await userEvent.type(await screen.findByLabelText("Commit SHA for orders"), SHA);
  await userEvent.click(screen.getByRole("button", { name: "Deploy orders" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("limit of 100");
});

test("polls every 5 s while a run is active and stops when it is done", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let phase = "RUNNING";
  const f = base({ "GET /api/namespaces/team-a/pipelines/orders/runs": () => ({ body: { runs: [run({ phase })] } }) }, { pipes: [pipe()] });
  const count = () => f.mock.calls.filter(([p]) => p === "/api/namespaces/team-a/pipelines/orders/runs").length;
  await openPipelines();
  expect(await screen.findByText("RUNNING")).toBeInTheDocument();
  const n = count();
  await act(async () => {
    vi.advanceTimersByTime(5000);
  });
  await waitFor(() => expect(count()).toBe(n + 1));
  phase = "SUCCEEDED";
  await act(async () => {
    vi.advanceTimersByTime(5000);
  });
  expect(await screen.findByText("SUCCEEDED")).toBeInTheDocument();
  const done = count();
  await act(async () => {
    vi.advanceTimersByTime(30000);
  });
  expect(count()).toBe(done);
});

test("polls the pipelines while one is building, and not when all are settled", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let phase = "BUILDING";
  const f = base({ "GET /api/namespaces/team-a/pipelines": () => ({ body: { pipelines: [pipe({ phase })] } }) });
  const count = () => f.mock.calls.filter(([p]) => p === "/api/namespaces/team-a/pipelines").length;
  await openPipelines();
  expect(await screen.findByText("BUILDING")).toBeInTheDocument();
  phase = "READY";
  await act(async () => {
    vi.advanceTimersByTime(5000);
  });
  expect(await screen.findByText("READY")).toBeInTheDocument();
  const n = count();
  await act(async () => {
    vi.advanceTimersByTime(30000);
  });
  expect(count()).toBe(n);
});
