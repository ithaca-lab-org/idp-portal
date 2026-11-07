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
    // a 204 has no body
    return new Response(status === 204 ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

const user = { sub: "u1", username: "user03", email: "u@x.com", groups: ["payment-bu"] };
const bucket = (o: object = {}) => ({
  namespace: "team-a", name: "data", bucketName: "idp-team-a-data-0123abcd", status: "COMPLETE", ...o,
});
const grant = (o: object = {}) => ({
  namespace: "team-a", name: "orders-data", workload: "orders", bucket: "data", access: "reader", status: "COMPLETE", ...o,
});
const info = (o: object = {}) => ({
  namespaces: [{ name: "team-a", businessUnit: "payment-bu" }],
  maxNamespaceLength: 31, maxNameLength: 18, maxBuckets: 3, maxGrants: 10,
  access: [{ value: "reader", label: "can read objects" }, { value: "writer", label: "can read and write objects" }],
  ...o,
});

function base(extra: Record<string, Route> = {}, items: () => object[] = () => []) {
  return mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
    "GET /api/resources/namespaces": () => ({ body: info() }),
    "GET /api/buckets?namespace=team-a": () => ({ body: { buckets: items() } }),
    ...extra,
  });
}

async function openResources() {
  render(<App />);
  await userEvent.click(await screen.findByRole("button", { name: "Resources" }));
}

async function openDetail() {
  await openResources();
  await userEvent.click(await screen.findByRole("button", { name: "Details of data" }));
}

afterEach(() => vi.unstubAllGlobals());

test("the Resources page is hidden when the server has it turned off", async () => {
  base({ "GET /api/resources/namespaces": () => ({ status: 404, body: { error: "not found" } }) });
  render(<App />);
  expect(await screen.findByText("No orders yet.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Resources" })).not.toBeInTheDocument();
});

test("lists buckets with their status and reason", async () => {
  base({}, () => [bucket(), bucket({ name: "logs", bucketName: "", status: "FAILED", reason: "NameTaken" })]);
  await openResources();
  const row = (await screen.findByText("data")).closest("tr")!;
  expect(within(row).getByText("COMPLETE")).toBeInTheDocument();
  expect(within(screen.getByText("logs").closest("tr")!).getByText("FAILED (NameTaken)")).toBeInTheDocument();
});

test("namespaces too long for a bucket name are disabled and explained", async () => {
  const long = "n".repeat(32);
  base({
    "GET /api/resources/namespaces": () => ({
      body: info({ namespaces: [{ name: long, businessUnit: "payment-bu" }, { name: "team-a", businessUnit: "payment-bu" }] }),
    }),
  });
  await openResources();
  const option = await screen.findByRole("option", { name: new RegExp(`${long}.*name too long for buckets`) });
  expect(option).toBeDisabled();
  expect(screen.getByRole("option", { name: "team-a (payment-bu)" })).toBeEnabled();
  expect(screen.getByText(/at most 31 characters/)).toBeInTheDocument();
  // the first usable namespace is the one listed
  expect(await screen.findByText("No buckets in team-a yet.")).toBeInTheDocument();
});

test("a page whose namespaces are all too long offers no order form", async () => {
  base({ "GET /api/resources/namespaces": () => ({ body: info({ namespaces: [{ name: "n".repeat(35), businessUnit: "payment-bu" }] }) }) });
  await openResources();
  expect(await screen.findByText("None of your namespaces can own buckets.")).toBeInTheDocument();
  expect(screen.queryByLabelText("Bucket name")).not.toBeInTheDocument();
});

test("ordering posts only the namespace and name, and checks the rules first", async () => {
  let items: object[] = [];
  const f = base(
    {
      "POST /api/orders/buckets": () => {
        items = [bucket({ bucketName: "", status: "PENDING" })];
        return { status: 202, body: items[0] };
      },
    },
    () => items,
  );
  await openResources();
  const input = await screen.findByLabelText("Bucket name");
  for (const bad of ["goog-data", "my-google", "data-", "Data"]) {
    await userEvent.clear(input);
    await userEvent.type(input, bad);
    await userEvent.click(screen.getByRole("button", { name: "Order bucket" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  }
  expect(f.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);

  await userEvent.clear(input);
  await userEvent.type(input, "data");
  await userEvent.click(screen.getByRole("button", { name: "Order bucket" }));
  expect(await screen.findByText(/Ordered data/)).toBeInTheDocument();
  const post = f.mock.calls.find(([, init]) => init?.method === "POST")!;
  expect(JSON.parse(post[1]!.body as string)).toEqual({ namespace: "team-a", name: "data" });
  expect(await screen.findByText("PENDING")).toBeInTheDocument();
});

test("shows the API's error for a duplicate and for the cap", async () => {
  base({ "POST /api/orders/buckets": () => ({ status: 409, body: { error: "this namespace has reached its limit of 3 buckets" } }) });
  await openResources();
  await userEvent.type(await screen.findByLabelText("Bucket name"), "data");
  await userEvent.click(screen.getByRole("button", { name: "Order bucket" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("limit of 3 buckets");
});

test("a list failure is shown", async () => {
  base({ "GET /api/buckets?namespace=team-a": () => ({ status: 502, body: { error: "the cluster request failed, try again" } }) });
  await openResources();
  expect(await screen.findByRole("alert")).toHaveTextContent("the cluster request failed");
});

test("the detail page shows gs://, copies it and lists the grants", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...bucket(), grants: [grant(), grant({ name: "worker-data", workload: "worker", access: "writer" })] } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [] } }),
    },
    () => [bucket()],
  );
  await openDetail();
  expect(await screen.findByText("gs://idp-team-a-data-0123abcd")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Copy bucket URI" }));
  await waitFor(() => expect(writeText).toHaveBeenCalledWith("gs://idp-team-a-data-0123abcd"));
  const row = screen.getByText("worker").closest("tr")!;
  expect(within(row).getByText("can read and write objects")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Back to buckets" }));
  expect(await screen.findByRole("button", { name: "Details of data" })).toBeInTheDocument();
});

test("a pending bucket has nothing to copy yet (no copy button)", async () => {
  const pending = bucket({ bucketName: "", status: "PENDING" });
  base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...pending, grants: [] } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [] } }),
    },
    () => [pending],
  );
  await openDetail();
  expect(await screen.findByText("Not created yet.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Copy bucket URI" })).not.toBeInTheDocument();
  expect(screen.getByText("No workload has access to this bucket yet.")).toBeInTheDocument();
});

test("a failed bucket shows the portal's message, not a cloud error", async () => {
  const failed = bucket({ bucketName: "", status: "FAILED", reason: "CreateFailed", message: "The platform could not complete this request. Ask the platform team." });
  base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...failed, grants: [] } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [] } }),
    },
    () => [failed],
  );
  await openDetail();
  expect(await screen.findByText(/FAILED \(CreateFailed\): The platform could not complete/)).toBeInTheDocument();
});

test("the grant form posts the workload, bucket and access, and refreshes the table", async () => {
  let grants: object[] = [];
  const f = base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...bucket(), grants } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [{ name: "orders" }, { name: "worker" }] } }),
      "POST /api/orders/grants": () => {
        grants = [grant({ workload: "worker", name: "worker-data", access: "writer", status: "PENDING" })];
        return { status: 202, body: grants[0] };
      },
    },
    () => [bucket()],
  );
  await openDetail();
  await userEvent.selectOptions(await screen.findByLabelText("Workload"), "worker");
  await userEvent.selectOptions(screen.getByLabelText("Access"), "writer");
  await userEvent.click(screen.getByRole("button", { name: "Give access" }));
  expect(await screen.findByText(/Giving worker access to data/)).toBeInTheDocument();
  const post = f.mock.calls.find(([, init]) => init?.method === "POST")!;
  expect(JSON.parse(post[1]!.body as string)).toEqual({ namespace: "team-a", workload: "worker", bucket: "data", access: "writer" });
  expect(await screen.findByText("PENDING")).toBeInTheDocument();
});

test("the grant form says so when the namespace has no workload", async () => {
  base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...bucket(), grants: [] } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [] } }),
    },
    () => [bucket()],
  );
  await openDetail();
  expect(await screen.findByText(/no workloads in team-a/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Give access" })).not.toBeInTheDocument();
});

test("the grant form shows the API's error", async () => {
  base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...bucket(), grants: [] } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [{ name: "orders" }] } }),
      "POST /api/orders/grants": () => ({ status: 409, body: { error: "this workload already has access to that bucket; revoke it first to change the access" } }),
    },
    () => [bucket()],
  );
  await openDetail();
  await userEvent.click(await screen.findByRole("button", { name: "Give access" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("already has access");
});

test("revoking needs a confirm step and can be cancelled", async () => {
  let grants: object[] = [grant()];
  const f = base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...bucket(), grants } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [{ name: "orders" }] } }),
      "DELETE /api/grants/team-a/orders-data": () => {
        grants = [];
        return { status: 204 };
      },
    },
    () => [bucket()],
  );
  await openDetail();
  await userEvent.click(await screen.findByRole("button", { name: "Revoke access of orders" }));
  // nothing is deleted until the confirm click
  expect(f.mock.calls.some(([, init]) => init?.method === "DELETE")).toBe(false);
  await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(screen.getByRole("button", { name: "Revoke access of orders" })).toBeInTheDocument();
  expect(f.mock.calls.some(([, init]) => init?.method === "DELETE")).toBe(false);

  await userEvent.click(screen.getByRole("button", { name: "Revoke access of orders" }));
  await userEvent.click(screen.getByRole("button", { name: "Confirm revoke of orders" }));
  expect(await screen.findByText("Access is being removed.")).toBeInTheDocument();
  expect(await screen.findByText("No workload has access to this bucket yet.")).toBeInTheDocument();
  expect(f.mock.calls.filter(([, init]) => init?.method === "DELETE")).toHaveLength(1);
});

test("a failed revoke shows the error and there is no bucket delete", async () => {
  base(
    {
      "GET /api/buckets/team-a/data": () => ({ body: { ...bucket(), grants: [grant()] } }),
      "GET /api/workloads?namespace=team-a": () => ({ body: { workloads: [{ name: "orders" }] } }),
      "DELETE /api/grants/team-a/orders-data": () => ({ status: 502, body: { error: "the cluster request failed, try again" } }),
    },
    () => [bucket()],
  );
  await openDetail();
  await userEvent.click(await screen.findByRole("button", { name: "Revoke access of orders" }));
  await userEvent.click(screen.getByRole("button", { name: "Confirm revoke of orders" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("the cluster request failed");
  expect(screen.queryByRole("button", { name: /delete bucket|remove bucket/i })).not.toBeInTheDocument();
});
