import { useEffect, useState } from "react";
import { businessUnits, listNamespaces, logout, me, type Namespace, type Order, type User } from "./api";
import { OrderForm } from "./OrderForm";
import { OrderHistory } from "./OrderHistory";
import { SecretsPage } from "./SecretsPage";

type Session =
  | { state: "loading" }
  | { state: "anonymous" }
  | { state: "error" }
  // namespaces is null when the Secrets page is turned off on the server
  | { state: "in"; user: User; units: string[]; namespaces: Namespace[] | null };

/** The Secrets page is optional: any failure to list namespaces just hides it. */
async function secretNamespaces(): Promise<Namespace[] | null> {
  try {
    return await listNamespaces();
  } catch {
    return null;
  }
}

export function App() {
  const [session, setSession] = useState<Session>({ state: "loading" });
  const [fresh, setFresh] = useState<Order | null>(null);
  const [signOutError, setSignOutError] = useState(false);
  const [page, setPage] = useState<"orders" | "secrets">("orders");

  useEffect(() => {
    (async () => {
      try {
        const user = await me();
        if (!user) return setSession({ state: "anonymous" });
        const [units, namespaces] = await Promise.all([businessUnits(), secretNamespaces()]);
        setSession({ state: "in", user, units, namespaces });
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
      {session.namespaces !== null && (
        <nav aria-label="Pages">
          <button aria-current={page === "orders" ? "page" : undefined} onClick={() => setPage("orders")}>
            Orders
          </button>
          <button aria-current={page === "secrets" ? "page" : undefined} onClick={() => setPage("secrets")}>
            Secrets
          </button>
        </nav>
      )}
      {signOutError && <p role="alert">Sign out failed. You are still signed in; try again.</p>}
      {page === "secrets" && session.namespaces !== null ? (
        <SecretsPage namespaces={session.namespaces} />
      ) : (
        <>
          <OrderForm units={session.units} onPlaced={setFresh} />
          <OrderHistory fresh={fresh} />
        </>
      )}
    </main>
  );
}
