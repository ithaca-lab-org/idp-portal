import { render, screen, waitFor, within } from "@testing-library/react";
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

const user = { sub: "u1", username: "user03", email: "u@x.com", groups: ["payment-bu"] };
const wl = (o: object = {}) => ({
  namespace: "team-a", name: "orders", serviceAccount: "orders",
  spiffeId: "spiffe://idp.example/ns/team-a/sa/orders", hostname: "orders.team-a.apps.example",
  status: "COMPLETE", conditions: [{ type: "Synced", status: "True", reason: "Synced", message: "all objects exist" }], ...o,
});

function base(extra: Record<string, Route> = {}, items: () => object[] = () => []) {
  return mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
    "GET /api/workloads/namespaces": () => ({
      body: {
        namespaces: [{ name: "team-a", businessUnit: "payment-bu" }],
        reservedNames: ["vault-reader", "default"],
        reservedPrefixes: ["default-token", "kube-", "idp-"],
      },
    }),
    "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: items() } }),
    ...extra,
  });
}

async function openWorkloads() {
  render(<App />);
  await userEvent.click(await screen.findByRole("button", { name: "Workloads" }));
}

afterEach(() => vi.unstubAllGlobals());

test("the Workloads page is hidden when the server has it turned off", async () => {
  base({ "GET /api/workloads/namespaces": () => ({ status: 404, body: { error: "not found" } }) });
  render(<App />);
  expect(await screen.findByText("No orders yet.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Workloads" })).not.toBeInTheDocument();
});

test("lists workloads with status, SPIFFE ID and hostname", async () => {
  base({}, () => [wl()]);
  await openWorkloads();
  const row = (await screen.findByText("orders")).closest("tr")!;
  expect(within(row).getByText("COMPLETE")).toBeInTheDocument();
  expect(within(row).getByText("spiffe://idp.example/ns/team-a/sa/orders")).toBeInTheDocument();
  expect(within(row).getByText("orders.team-a.apps.example")).toBeInTheDocument();
});

test("ordering posts only the namespace and name, and checks the rules first", async () => {
  let items: object[] = [];
  const f = base(
    {
      "POST /api/orders/workloads": () => {
        items = [wl({ status: "PENDING", spiffeId: "", hostname: "", conditions: [] })];
        return { status: 202, body: items[0] };
      },
    },
    () => items,
  );
  await openWorkloads();
  const input = await screen.findByLabelText("Workload name");
  await userEvent.type(input, "kube-x");
  await userEvent.click(screen.getByRole("button", { name: "Order workload" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("kube- are reserved");
  expect(f.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);

  await userEvent.clear(input);
  await userEvent.type(input, "orders");
  await userEvent.click(screen.getByRole("button", { name: "Order workload" }));
  expect(await screen.findByText(/Ordered orders/)).toBeInTheDocument();
  const post = f.mock.calls.find(([, init]) => init?.method === "POST")!;
  expect(JSON.parse(post[1]!.body as string)).toEqual({ namespace: "team-a", name: "orders" });
  expect(await screen.findByText("PENDING")).toBeInTheDocument();
});

test("shows the API's error for a duplicate", async () => {
  base({ "POST /api/orders/workloads": () => ({ status: 409, body: { error: "a workload with this name already exists in that namespace" } }) });
  await openWorkloads();
  await userEvent.type(await screen.findByLabelText("Workload name"), "orders");
  await userEvent.click(screen.getByRole("button", { name: "Order workload" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("already exists");
});

test("the detail page shows the identity, conditions and copies on request", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  base({ "GET /api/workloads/team-a/orders": () => ({ body: wl() }) }, () => [wl()]);
  await openWorkloads();
  await userEvent.click(await screen.findByRole("button", { name: "Details of orders" }));
  expect(await screen.findByText("all objects exist")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Copy SPIFFE ID" }));
  await waitFor(() => expect(writeText).toHaveBeenCalledWith("spiffe://idp.example/ns/team-a/sa/orders"));
  await userEvent.click(screen.getByRole("button", { name: "Copy hostname" }));
  await waitFor(() => expect(writeText).toHaveBeenCalledWith("orders.team-a.apps.example"));
  await userEvent.click(screen.getByRole("button", { name: "Back to workloads" }));
  expect(await screen.findByRole("button", { name: "Details of orders" })).toBeInTheDocument();
});

test("a pending workload has nothing to copy yet", async () => {
  const pending = wl({ status: "PENDING", spiffeId: "", hostname: "", conditions: [] });
  base({ "GET /api/workloads/team-a/orders": () => ({ body: pending }) }, () => [pending]);
  await openWorkloads();
  await userEvent.click(await screen.findByRole("button", { name: "Details of orders" }));
  expect(await screen.findByText("Not issued yet.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Copy SPIFFE ID" })).toBeDisabled();
  expect(screen.getByText("The operator has not reported any conditions yet.")).toBeInTheDocument();
});
