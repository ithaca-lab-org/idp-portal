import { useState } from "react";
import { ApiError, placeOrder, type Order } from "./api";

const NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

export function OrderForm({ units, onPlaced }: { units: string[]; onPlaced: (o: Order) => void }) {
  const [name, setName] = useState("");
  const [unit, setUnit] = useState(units[0] ?? "");
  const [size, setSize] = useState<"small" | "medium">("small");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  if (units.length === 0) {
    return <p>You are not in any business unit, so you cannot order namespaces. Ask an administrator.</p>;
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (name.length > 40 || !NAME_RE.test(name)) {
      setError("Name must be a lowercase DNS label of at most 40 characters (a-z, 0-9, -).");
      return;
    }
    setError("");
    setBusy(true);
    try {
      onPlaced(await placeOrder({ name, businessUnit: unit, size }));
      setName("");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not place the order.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit}>
      <h2>Order a namespace</h2>
      <label>
        Name
        <input value={name} onChange={(e) => setName(e.target.value)} maxLength={40} required />
      </label>
      <label>
        Business unit
        <select value={unit} onChange={(e) => setUnit(e.target.value)}>
          {units.map((u) => (
            <option key={u}>{u}</option>
          ))}
        </select>
      </label>
      <label>
        Size
        <select value={size} onChange={(e) => setSize(e.target.value as "small" | "medium")}>
          <option value="small">small</option>
          <option value="medium">medium</option>
        </select>
      </label>
      <button type="submit" disabled={busy}>
        {busy ? "Ordering…" : "Place order"}
      </button>
      {error && <p role="alert">{error}</p>}
    </form>
  );
}
