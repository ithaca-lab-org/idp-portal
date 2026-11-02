import { useEffect, useState } from "react";
import { businessUnits, listNamespaces, listWorkloadNamespaces, logout, me, type Order, type SecretsInfo, type User } from "./api";
import { OrderForm } from "./OrderForm";
import { OrderHistory } from "./OrderHistory";
import { SecretsPage } from "./SecretsPage";
import { WorkloadsPage } from "./WorkloadsPage";

type Session =
  | { state: "loading" }
  | { state: "anonymous" }
  | { state: "error" }
  // secrets is null when the Secrets page is turned off on the server
  // workloads is null when the Workloads page is turned off on the server
  | { state: "in"; user: User; units: string[]; secrets: SecretsInfo | null; workloads: SecretsInfo | null };

/** The Secrets page is optional: any failure to list namespaces just hides it. */
async function secretNamespaces(): Promise<SecretsInfo | null> {
  try {
    return await listNamespaces();
  } catch {
    return null;
  }
}

/** The Workloads page is optional too. */
async function workloadNamespaces(): Promise<SecretsInfo | null> {
  try {
    return await listWorkloadNamespaces();
  } catch {
    return null;
  }
}

export function App() {
  const [session, setSession] = useState<Session>({ state: "loading" });
  const [fresh, setFresh] = useState<Order | null>(null);
  const [signOutError, setSignOutError] = useState(false);
  const [page, setPage] = useState<"orders" | "secrets" | "workloads">("orders");

  useEffect(() => {
    (async () => {
      try {
        const user = await me();
        if (!user) return setSession({ state: "anonymous" });
        const [units, secrets, workloads] = await Promise.all([businessUnits(), secretNamespaces(), workloadNamespaces()]);
        setSession({ state: "in", user, units, secrets, workloads });
      } catch {
        setSession({ state: "error" });
      }
    })();
  }, []);

  async function signOut() {
    try {
      const url = await logout();
      // only follow an https URL from our own API
      window.location.assign(url && url.startsWith("https://") ? url : "/");
    } catch {
      setSignOutError(true);
    }
  }

  if (session.state === "loading") return <p>Loading…</p>;
  if (session.state === "error") return <p role="alert">The portal is unavailable. Try again shortly.</p>;
  if (session.state === "anonymous") {
    return (
      <main>
        <h1>IDP portal</h1>
        <a href="/auth/login">Sign in</a>
      </main>
    );
  }
  return (
    <main>
      <header>
        <h1>IDP portal</h1>
        <span>
          Signed in as <strong>{session.user.username}</strong>
        </span>
        <button onClick={signOut}>Sign out</button>
      </header>
      {(session.secrets !== null || session.workloads !== null) && (
        <nav aria-label="Pages">
          <button aria-current={page === "orders" ? "page" : undefined} onClick={() => setPage("orders")}>
            Orders
          </button>
          {session.secrets !== null && (
            <button aria-current={page === "secrets" ? "page" : undefined} onClick={() => setPage("secrets")}>
              Secrets
            </button>
          )}
          {session.workloads !== null && (
            <button aria-current={page === "workloads" ? "page" : undefined} onClick={() => setPage("workloads")}>
              Workloads
            </button>
          )}
        </nav>
      )}
      {signOutError && <p role="alert">Sign out failed. You are still signed in; try again.</p>}
      {page === "secrets" && session.secrets !== null ? (
        <SecretsPage namespaces={session.secrets.namespaces} rules={session.secrets} />
      ) : page === "workloads" && session.workloads !== null ? (
        <WorkloadsPage namespaces={session.workloads.namespaces} rules={session.workloads} />
      ) : (
        <>
          <OrderForm units={session.units} onPlaced={setFresh} />
          <OrderHistory fresh={fresh} />
        </>
      )}
    </main>
  );
}
