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
    return new Response(status === 204 ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

const VALUE = "hunter2-top-secret";
const user = { sub: "u1", username: "user03", email: "u@x.com", groups: ["payment-bu"] };
const meta = (o: object = {}) => ({
  name: "db", keys: ["DB_PASSWORD"], version: 1, updatedBy: "u@x.com", updatedAt: "2026-10-29T00:00:00Z", ...o,
});

function base(extra: Record<string, Route> = {}, secrets: () => object[] = () => []) {
  return mockFetch({
    "GET /auth/me": () => ({ body: user }),
    "GET /api/business-units": () => ({ body: { businessUnits: user.groups } }),
    "GET /api/orders": () => ({ body: { orders: [] } }),
    "GET /api/namespaces": () => ({
      body: {
        namespaces: [{ name: "team-a", businessUnit: "payment-bu" }],
        reservedNames: ["vault-reader", "default"],
        reservedPrefixes: ["default-token", "kube-", "idp-"],
      },
    }),
    "GET /api/namespaces/team-a/secrets": () => ({ body: { secrets: secrets() } }),
    ...extra,
  });
}

async function openSecrets() {
  render(<App />);
  await userEvent.click(await screen.findByRole("button", { name: "Secrets" }));
}

afterEach(() => vi.unstubAllGlobals());

test("the Secrets page is hidden when the server has it turned off", async () => {
  base({ "GET /api/namespaces": () => ({ status: 404, body: { error: "not found" } }) });
  render(<App />);
  expect(await screen.findByText("No orders yet.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Secrets" })).not.toBeInTheDocument();
});

test("lists secrets with key names, version and who changed them", async () => {
  base({}, () => [meta({ keys: ["A", "B"], version: 3 })]);
  await openSecrets();
  const row = (await screen.findByText("A, B")).closest("tr")!;
  expect(within(row).getByText("db")).toBeInTheDocument();
  expect(within(row).getByText("3")).toBeInTheDocument();
  expect(within(row).getByText("u@x.com")).toBeInTheDocument();
  expect(screen.getByRole("complementary", { name: "How to use" })).toHaveTextContent("secretRef:");
  expect(screen.getByRole("complementary", { name: "How to use" })).toHaveTextContent("name: db");
  expect(screen.getByRole("complementary", { name: "How to use" })).toHaveTextContent("restart");
});

test("a new secret is sent once, inputs are masked, and the value is never shown again", async () => {
  let list: object[] = [];
  const f = base(
    {
      "PUT /api/namespaces/team-a/secrets/db?create=true": () => {
        list = [meta({ keys: ["DB_PASSWORD", "DB_USER"] })];
        return { body: { name: "db", keys: ["DB_PASSWORD", "DB_USER"], version: 1 } };
      },
    },
    () => list,
  );
  await openSecrets();
  await userEvent.click(await screen.findByRole("button", { name: "New secret" }));
  await userEvent.type(screen.getByLabelText("Secret name"), "db");
  await userEvent.type(screen.getByLabelText("Key 1"), "DB_PASSWORD");
  const v1 = screen.getByLabelText("Value 1");
  expect(v1).toHaveAttribute("type", "password");
  await userEvent.type(v1, VALUE);
  await userEvent.click(screen.getByRole("button", { name: "Add key" }));
  await userEvent.type(screen.getByLabelText("Key 2"), "DB_USER");
  expect(screen.getByLabelText("Value 2")).toHaveAttribute("type", "password");
  await userEvent.type(screen.getByLabelText("Value 2"), "app");
  await userEvent.click(screen.getByRole("button", { name: "Save secret" }));

  expect(await screen.findByRole("status")).toHaveTextContent("Saved db (version 1");
  const put = f.mock.calls.find(([, i]) => i?.method === "PUT")!;
  expect(JSON.parse(put[1]!.body as string)).toEqual({ data: { DB_PASSWORD: VALUE, DB_USER: "app" } });
  expect(await screen.findByText("DB_PASSWORD, DB_USER")).toBeInTheDocument();
  // the form is gone and no input or text holds the value
  expect(screen.queryByLabelText("Value 1")).not.toBeInTheDocument();
  expect(document.body.innerHTML).not.toContain(VALUE);
  for (const input of Array.from(document.querySelectorAll("input"))) expect(input.value).not.toBe(VALUE);
});

test("replace values pre-fills key names only, delete asks first", async () => {
  let list = [meta({ keys: ["TOKEN"] })];
  const f = base(
    {
      "PUT /api/namespaces/team-a/secrets/db": () => ({ body: { name: "db", keys: ["TOKEN"], version: 2 } }),
      "DELETE /api/namespaces/team-a/secrets/db": () => {
        list = [];
        return { status: 204 };
      },
    },
    () => list,
  );
  await openSecrets();
  await userEvent.click(await screen.findByRole("button", { name: "Replace values of db" }));
  expect(screen.getByLabelText("Key 1")).toHaveValue("TOKEN");
  expect(screen.getByLabelText("Value 1")).toHaveValue("");
  await userEvent.type(screen.getByLabelText("Value 1"), VALUE);
  await userEvent.click(screen.getByRole("button", { name: "Save secret" }));
  expect(await screen.findByRole("status")).toHaveTextContent("version 2");
  expect(document.body.innerHTML).not.toContain(VALUE);

  await userEvent.click(screen.getByRole("button", { name: "Delete db" }));
  expect(f.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(false);
  await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(f.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(false);
  await userEvent.click(screen.getByRole("button", { name: "Delete db" }));
  await userEvent.click(screen.getByRole("button", { name: "Yes, delete db" }));
  expect(await screen.findByText("No secrets in team-a yet.")).toBeInTheDocument();
  expect(f.mock.calls.filter(([, i]) => i?.method === "DELETE")).toHaveLength(1);
});

test("server errors are shown and bad input is rejected before sending", async () => {
  const f = base({
    "PUT /api/namespaces/team-a/secrets/db?create=true": () => ({ status: 502, body: { error: "the secret backend is unavailable, try again" } }),
  });
  await openSecrets();
  await userEvent.click(await screen.findByRole("button", { name: "New secret" }));
  // the reserved names come from the API
  await userEvent.type(screen.getByLabelText("Secret name"), "idp-x");
  await userEvent.type(screen.getByLabelText("Key 1"), "K");
  await userEvent.type(screen.getByLabelText("Value 1"), "v");
  await userEvent.click(screen.getByRole("button", { name: "Save secret" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Names starting with idp- are reserved");
  await userEvent.clear(screen.getByLabelText("Secret name"));
  await userEvent.type(screen.getByLabelText("Secret name"), "default");
  await userEvent.click(screen.getByRole("button", { name: "Save secret" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("The name default is reserved"));
  await userEvent.clear(screen.getByLabelText("Key 1"));
  await userEvent.clear(screen.getByLabelText("Value 1"));
  await userEvent.clear(screen.getByLabelText("Secret name"));
  await userEvent.type(screen.getByLabelText("Secret name"), "db");
  await userEvent.type(screen.getByLabelText("Key 1"), "1BAD");
  await userEvent.type(screen.getByLabelText("Value 1"), VALUE);
  await userEvent.click(screen.getByRole("button", { name: "Save secret" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Key names");
  expect(f.mock.calls.some(([, i]) => i?.method === "PUT")).toBe(false);

  await userEvent.clear(screen.getByLabelText("Key 1"));
  await userEvent.type(screen.getByLabelText("Key 1"), "GOOD");
  await userEvent.click(screen.getByRole("button", { name: "Save secret" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("secret backend is unavailable"));
  expect(screen.getByRole("alert")).not.toHaveTextContent(VALUE);
});

test("New secret is create-only: an existing name gives a conflict and nothing is replaced", async () => {
  const f = base({
    "PUT /api/namespaces/team-a/secrets/db?create=true": () => ({
      status: 409,
      body: { error: "a secret with this name already exists; use Replace values" },
    }),
  });
  await openSecrets();
  await userEvent.click(await screen.findByRole("button", { name: "New secret" }));
  await userEvent.type(screen.getByLabelText("Secret name"), "db");
  await userEvent.type(screen.getByLabelText("Key 1"), "K");
  await userEvent.type(screen.getByLabelText("Value 1"), VALUE);
  await userEvent.click(screen.getByRole("button", { name: "Save secret" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("already exists");
  expect(f.mock.calls.filter(([p]) => p === "/api/namespaces/team-a/secrets/db")).toHaveLength(0);
});
