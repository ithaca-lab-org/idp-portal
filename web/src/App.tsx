import { useEffect, useState } from "react";
import { businessUnits, logout, me, type Order, type User } from "./api";
import { OrderForm } from "./OrderForm";
import { OrderHistory } from "./OrderHistory";

type Session = { state: "loading" } | { state: "anonymous" } | { state: "error" } | { state: "in"; user: User; units: string[] };

export function App() {
  const [session, setSession] = useState<Session>({ state: "loading" });
  const [fresh, setFresh] = useState<Order | null>(null);
  const [signOutError, setSignOutError] = useState(false);

  useEffect(() => {
    (async () => {
      try {
        const user = await me();
        if (!user) return setSession({ state: "anonymous" });
        setSession({ state: "in", user, units: await businessUnits() });
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
      {signOutError && <p role="alert">Sign out failed. You are still signed in; try again.</p>}
      <OrderForm units={session.units} onPlaced={setFresh} />
      <OrderHistory fresh={fresh} />
    </main>
  );
}
