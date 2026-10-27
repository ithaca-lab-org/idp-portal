import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import { App } from "./App";

type Route = (init?: RequestInit) => { status?: number; body: unknown };

function mockFetch(routes: Record<string, Route>) {
  const f = vi.fn(async (path: string, init?: RequestInit) => {
    const r = routes[`${init?.method ?? "GET"} ${path}`];
    if (!r) throw new Error(`unexpected ${path}`);
    const { status = 200, body } = r(init);
    return new Response(JSON.stringify(body), { status });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

const user = { sub: "u1", username: "user03", email: "u@x.com", groups: ["payment-bu", "cloudspend-bu"] };
const order = (o: object) => ({
  id: "ORD-2026-000001", kind: "namespace", name: "team-a", businessUnit: "payment-bu", owner: "u@x.com",
  size: "small", status: "PENDING", createdAt: "2026-10-27T00:00:00Z", updatedAt: "2026-10-27T00:00:00Z", ...o,
});

afterEach(() => vi.unstubAllGlobals());

test("anonymous visitors see a sign-in link", async () => {
  mockFetch({ "GET /auth/me": () => ({ status: 401, body: { error: "unauthorized" } }) });
  render(<App />);
  expect(await screen.findByRole("link", { name: "Sign in" })).toHaveAttribute("href", "/auth/login");
});

test("the form offers only the user's business units", async () => {
  mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
  });
  render(<App />);
  const select = await screen.findByLabelText("Business unit");
  expect(Array.from(select.querySelectorAll("option")).map((o) => o.textContent)).toEqual(user.groups);
  expect(await screen.findByText("No orders yet.")).toBeInTheDocument();
});

test("placing an order posts JSON and refreshes the history", async () => {
  let orders: object[] = [];
  const f = mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders } }),
    "POST /api/orders/namespaces": () => {
      orders = [order({})];
      return { status: 202, body: order({}) };
    },
  });
  render(<App />);
  await userEvent.type(await screen.findByLabelText("Name"), "team-a");
  await userEvent.click(screen.getByRole("button", { name: "Place order" }));
  expect(await screen.findByText("ORD-2026-000001")).toBeInTheDocument();
  const post = f.mock.calls.find(([, i]) => i?.method === "POST")!;
  expect(JSON.parse(post[1]!.body as string)).toEqual({ name: "team-a", businessUnit: "payment-bu", size: "small" });
});

test("server errors are shown, bad names are rejected before sending", async () => {
  const f = mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
    "POST /api/orders/namespaces": () => ({ status: 409, body: { error: "a namespace with that name already exists or is already ordered" } }),
  });
  render(<App />);
  const name = await screen.findByLabelText("Name");
  await userEvent.type(name, "Bad_Name");
  await userEvent.click(screen.getByRole("button", { name: "Place order" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("lowercase DNS label");
  expect(f.mock.calls.some(([, i]) => i?.method === "POST")).toBe(false);
  await userEvent.clear(name);
  await userEvent.type(name, "taken");
  await userEvent.click(screen.getByRole("button", { name: "Place order" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("already exists"));
});

test("a user without business units cannot order", async () => {
  mockFetch({
    "GET /auth/me": () => ({ body: { ...user, groups: [] } }),
    "GET /api/business-units": () => ({ body: { businessUnits: [] } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
  });
  render(<App />);
  expect(await screen.findByText(/not in any business unit/)).toBeInTheDocument();
});

test("a failed sign out is reported and keeps the user on the page", async () => {
  mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
    "POST /auth/logout": () => ({ status: 403, body: { error: "forbidden" } }),
  });
  render(<App />);
  await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Sign out failed");
});
