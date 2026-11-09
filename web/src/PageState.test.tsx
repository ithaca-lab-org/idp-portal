import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import { App } from "./App";

const user = { sub: "u1", username: "user03", email: "u@x.com", groups: ["payment-bu"] };
const rules = { reservedNames: [], reservedPrefixes: [] };
const nsA = { name: "team-a", businessUnit: "payment-bu" };
const nsB = { name: "team-b", businessUnit: "payment-bu" };
const wl = (namespace: string, name: string) => ({
  namespace, name, serviceAccount: name, spiffeId: `spiffe://x/${name}`, hostname: `${name}.x`, status: "COMPLETE", conditions: [],
});
const resInfo = (namespaces: object[]) => ({
  namespaces, maxNamespaceLength: 31, maxNameLength: 18, maxBuckets: 3, maxGrants: 10, access: [{ value: "reader", label: "r" }],
});

/** Serves the portal API. `wlNamespaces` is called on every namespace-list request, `hold` delays workload lists. */
function serve(opts: { wlNamespaces?: (call: number) => object[] | "fail"; hold?: (ns: string, call: number) => Promise<void> | undefined; name?: (ns: string, call: number) => string } = {}) {
  let wlCalls = 0;
  const lists = vi.fn();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string) => {
      const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
      if (path === "/auth/me") return json(user);
      if (path === "/api/business-units") return json({ businessUnits: user.groups });
      if (path === "/api/orders") return json({ orders: [] });
      if (path === "/api/workloads/namespaces") {
        const r = (opts.wlNamespaces ?? (() => [nsA, nsB]))(wlCalls++);
        return r === "fail" ? json({ error: "boom" }, 500) : json({ namespaces: r, ...rules });
      }
      if (path === "/api/resources/namespaces") return json(resInfo([nsA, nsB]));
      const w = /^\/api\/workloads\?namespace=(.+)$/.exec(path);
      if (w) {
        const call = lists.mock.calls.length;
        lists(w[1]);
        await opts.hold?.(w[1], call);
        return json({ workloads: [wl(w[1], opts.name?.(w[1], call) ?? `app-${w[1]}`)] });
      }
      if (path.startsWith("/api/buckets?")) return json({ buckets: [] });
      throw new Error(`unexpected ${path}`);
    }),
  );
  return { lists };
}

const nav = (name: string) => screen.findByRole("button", { name });
const section = (heading: string) => screen.getByRole("heading", { name: heading, level: 2 }).closest("section")!;

afterEach(() => vi.unstubAllGlobals());

test("Workloads keeps its namespace and shows the loaded list while it revalidates", async () => {
  let release: () => void = () => {};
  let gate = false;
  const { lists } = serve({ hold: () => (gate ? new Promise<void>((r) => (release = r)) : undefined) });
  render(<App />);
  await userEvent.click(await nav("Workloads"));
  await userEvent.selectOptions(await screen.findByLabelText("Namespace"), "team-b");
  expect(await screen.findByText("app-team-b")).toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "Orders" }));
  gate = true; // the revalidation stays pending
  const before = lists.mock.calls.length;
  await userEvent.click(screen.getByRole("button", { name: "Workloads" }));
  await waitFor(() => expect(lists.mock.calls.length).toBe(before + 1));
  expect(lists).toHaveBeenLastCalledWith("team-b");
  expect(screen.getByLabelText("Namespace")).toHaveValue("team-b");
  expect(within(section("Workloads")).getByText("app-team-b")).toBeInTheDocument();
  expect(within(section("Workloads")).queryByText("Loading…")).not.toBeInTheDocument();
  release();
});

test("a namespace that became ready after login appears when the page is opened", async () => {
  serve({ wlNamespaces: (call) => (call === 0 ? [nsA] : [nsA, nsB]) });
  render(<App />);
  await userEvent.click(await nav("Workloads"));
  expect(await screen.findByRole("option", { name: "team-b (payment-bu)" })).toBeInTheDocument();
});

test("the selected namespace is shared by the pages and survives navigation", async () => {
  serve();
  render(<App />);
  await userEvent.click(await nav("Workloads"));
  await userEvent.selectOptions(await screen.findByLabelText("Namespace"), "team-b");
  await userEvent.click(screen.getByRole("button", { name: "Resources" }));
  expect(await within(section("Resources")).findByLabelText("Namespace")).toHaveValue("team-b");
  await userEvent.click(screen.getByRole("button", { name: "Orders" }));
  await userEvent.click(screen.getByRole("button", { name: "Workloads" }));
  expect(within(section("Workloads")).getByLabelText("Namespace")).toHaveValue("team-b");
  expect(sessionStorage.getItem("idp.ns")).toBe("team-b");
});

test("a failed namespace refresh keeps the page usable", async () => {
  serve({ wlNamespaces: (call) => (call === 0 ? [nsA] : "fail") });
  render(<App />);
  await userEvent.click(await nav("Workloads"));
  expect(await screen.findByText("app-team-a")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Orders" }));
  await userEvent.click(screen.getByRole("button", { name: "Workloads" }));
  expect(await screen.findByText("app-team-a")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Workloads" })).toBeInTheDocument();
  expect(within(section("Workloads")).queryByRole("alert")).not.toBeInTheDocument();
});

test("a namespace removed by a refresh makes the page fall back to another one", async () => {
  serve({ wlNamespaces: (call) => (call < 2 ? [nsA, nsB] : [nsA]) });
  render(<App />);
  await userEvent.click(await nav("Workloads"));
  await userEvent.selectOptions(await screen.findByLabelText("Namespace"), "team-b");
  expect(await screen.findByText("app-team-b")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Orders" }));
  await userEvent.click(screen.getByRole("button", { name: "Workloads" }));
  expect(await screen.findByText("app-team-a")).toBeInTheDocument();
  expect(screen.getByLabelText("Namespace")).toHaveValue("team-a");
});

test("a slow answer for a namespace the user left does not replace the current list", async () => {
  let release: () => void = () => {};
  serve({ hold: (ns) => (ns === "team-a" ? new Promise<void>((r) => (release = r)) : undefined) });
  render(<App />);
  await userEvent.click(await nav("Workloads"));
  await userEvent.selectOptions(await screen.findByLabelText("Namespace"), "team-b");
  expect(await screen.findByText("app-team-b")).toBeInTheDocument();
  release(); // team-a answers last
  await new Promise((r) => setTimeout(r, 20));
  expect(screen.getByText("app-team-b")).toBeInTheDocument();
  expect(screen.queryByText("app-team-a")).not.toBeInTheDocument();
});

test("overlapping loads of one namespace: the latest request wins", async () => {
  let release: () => void = () => {};
  serve({
    hold: (_ns, call) => (call === 1 ? new Promise<void>((r) => (release = r)) : undefined),
    name: (_ns, call) => `data-${call}`,
  });
  render(<App />);
  await userEvent.click(await nav("Workloads"));
  expect(await screen.findByText("data-0")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Refresh" })); // call 1, held
  await userEvent.click(screen.getByRole("button", { name: "Refresh" })); // call 2, fast
  expect(await screen.findByText("data-2")).toBeInTheDocument();
  release();
  await new Promise((r) => setTimeout(r, 20));
  expect(screen.getByText("data-2")).toBeInTheDocument();
  expect(screen.queryByText("data-1")).not.toBeInTheDocument();
});
