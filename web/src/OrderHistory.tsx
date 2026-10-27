import { useEffect, useState } from "react";
import { listOrders, type Order } from "./api";

const POLL_MS = 5000;

/** Lists the user's orders and polls while any is still PENDING. `fresh` is an order just placed. */
export function OrderHistory({ fresh }: { fresh: Order | null }) {
  const [orders, setOrders] = useState<Order[] | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let stop = false;
    let timer: ReturnType<typeof setTimeout>;
    async function load() {
      try {
        const list = await listOrders();
        if (stop) return;
        setOrders(list);
        setError("");
        if (list.some((o) => o.status === "PENDING")) timer = setTimeout(load, POLL_MS);
      } catch {
        if (stop) return;
        setError("Could not load orders.");
        timer = setTimeout(load, POLL_MS * 3);
      }
    }
    load();
    return () => {
      stop = true;
      clearTimeout(timer);
    };
  }, [fresh]);

  return (
    <section>
      <h2>Your orders</h2>
      {error && <p role="alert">{error}</p>}
      {orders === null ? (
        <p>Loading…</p>
      ) : orders.length === 0 ? (
        <p>No orders yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Order</th>
              <th>Namespace</th>
              <th>Business unit</th>
              <th>Size</th>
              <th>Status</th>
              <th>Placed</th>
            </tr>
          </thead>
          <tbody>
            {orders.map((o) => (
              <tr key={o.id}>
                <td>{o.id}</td>
                <td>{o.name}</td>
                <td>{o.businessUnit}</td>
                <td>{o.size}</td>
                <td title={o.message}>{o.status}</td>
                <td>{new Date(o.createdAt).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
