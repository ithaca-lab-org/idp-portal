import { useCallback, useEffect, useRef, useState } from "react";
import {
  businessUnits,
  listNamespaces,
  listResourceNamespaces,
  listWorkloadNamespaces,
  logout,
  me,
  type Order,
  type ResourcesInfo,
  type SecretsInfo,
  type User,
} from "./api";
import { OrderForm } from "./OrderForm";
import { rememberedNs, rememberNs } from "./namespaces";
import { OrderHistory } from "./OrderHistory";
import { ResourcesPage } from "./ResourcesPage";
import { SecretsPage } from "./SecretsPage";
import { WorkloadsPage } from "./WorkloadsPage";

type Session =
  | { state: "loading" }
  | { state: "anonymous" }
  | { state: "error" }
  // secrets is null when the Secrets page is turned off on the server
  // workloads is null when the Workloads page is turned off on the server
  // resources is null when the Resources page is turned off on the server
  | { state: "in"; user: User; units: string[]; secrets: SecretsInfo | null; workloads: SecretsInfo | null; resources: ResourcesInfo | null };

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

/** The Resources page is optional too. */
async function resourceNamespaces(): Promise<ResourcesInfo | null> {
  try {
    return await listResourceNamespaces();
  } catch {
    return null;
  }
}

type Page = "orders" | "secrets" | "workloads" | "resources";
const REFRESH_MS = 30_000;

export function App() {
  const [session, setSession] = useState<Session>({ state: "loading" });
  const [fresh, setFresh] = useState<Order | null>(null);
  const [signOutError, setSignOutError] = useState(false);
  const [page, setPage] = useState<Page>("orders");
  // a page is mounted the first time it is visited and stays mounted (hidden) afterwards
  const [visited, setVisited] = useState<Page[]>(["orders"]);
  const [ns, setNs] = useState(rememberedNs);
  const lastRefresh = useRef(0);

  function chooseNs(next: string) {
    setNs(next);
    rememberNs(next);
  }

  /** Reloads one namespace list in the background. A failure keeps the old value; an unchanged list keeps the state. */
  const refresh = useCallback(async (key: "secrets" | "workloads" | "resources") => {
    lastRefresh.current = Date.now();
    try {
      const next = await (key === "secrets" ? listNamespaces : key === "workloads" ? listWorkloadNamespaces : listResourceNamespaces)();
      if (!next) return;
      setSession((s) => (s.state === "in" && JSON.stringify(s[key]) !== JSON.stringify(next) ? ({ ...s, [key]: next } as Session) : s));
    } catch {
      // keep what we have
    }
  }, []);

  function go(p: Page) {
    setPage(p);
    setVisited((v) => (v.includes(p) ? v : [...v, p]));
    if (p !== "orders") refresh(p);
  }

  // coming back to the browser tab refreshes the current page's namespaces, at most every 30 s
  const signedIn = session.state === "in";
  useEffect(() => {
    if (!signedIn || page === "orders") return;
    const onVisible = () => {
      if (document.visibilityState === "visible" && Date.now() - lastRefresh.current >= REFRESH_MS) refresh(page);
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [signedIn, page, refresh]);

  useEffect(() => {
    (async () => {
      try {
        const user = await me();
        if (!user) return setSession({ state: "anonymous" });
        const [units, secrets, workloads, resources] = await Promise.all([
          businessUnits(),
          secretNamespaces(),
          workloadNamespaces(),
          resourceNamespaces(),
        ]);
        setSession({ state: "in", user, units, secrets, workloads, resources });
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
      {(session.secrets !== null || session.workloads !== null || session.resources !== null) && (
        <nav aria-label="Pages">
          <button aria-current={page === "orders" ? "page" : undefined} onClick={() => go("orders")}>
            Orders
          </button>
          {session.secrets !== null && (
            <button aria-current={page === "secrets" ? "page" : undefined} onClick={() => go("secrets")}>
              Secrets
            </button>
          )}
          {session.workloads !== null && (
            <button aria-current={page === "workloads" ? "page" : undefined} onClick={() => go("workloads")}>
              Workloads
            </button>
          )}
          {session.resources !== null && (
            <button aria-current={page === "resources" ? "page" : undefined} onClick={() => go("resources")}>
              Resources
            </button>
          )}
        </nav>
      )}
      {signOutError && <p role="alert">Sign out failed. You are still signed in; try again.</p>}
      {session.secrets !== null && visited.includes("secrets") && (
        <div hidden={page !== "secrets"}>
          <SecretsPage namespaces={session.secrets.namespaces} rules={session.secrets} active={page === "secrets"} ns={ns} onNs={chooseNs} />
        </div>
      )}
      {session.workloads !== null && visited.includes("workloads") && (
        <div hidden={page !== "workloads"}>
          <WorkloadsPage namespaces={session.workloads.namespaces} rules={session.workloads} active={page === "workloads"} ns={ns} onNs={chooseNs} />
        </div>
      )}
      {session.resources !== null && visited.includes("resources") && (
        <div hidden={page !== "resources"}>
          <ResourcesPage info={session.resources} active={page === "resources"} ns={ns} onNs={chooseNs} />
        </div>
      )}
      <div hidden={page !== "orders"}>
        <OrderForm units={session.units} onPlaced={setFresh} />
        <OrderHistory fresh={fresh} active={page === "orders"} />
      </div>
    </main>
  );
}
