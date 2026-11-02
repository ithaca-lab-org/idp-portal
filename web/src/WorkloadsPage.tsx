import { useCallback, useEffect, useState } from "react";
import { ApiError, listWorkloads, placeWorkload, type NameRules, type Namespace, type Workload } from "./api";
import { WorkloadDetail } from "./WorkloadDetail";

const NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

// the same rules as the API and the WorkloadIdentity CRD; the reserved lists come from the API
function check(name: string, rules: NameRules): string {
  if (name.length > 40 || !NAME_RE.test(name)) return "Name must be a lowercase DNS label of at most 40 characters (a-z, 0-9, -).";
  if (rules.reservedNames.includes(name)) return `The name ${name} is reserved.`;
  const prefix = rules.reservedPrefixes.find((p) => name.startsWith(p));
  if (prefix) return `Names starting with ${prefix} are reserved.`;
  return "";
}

export function WorkloadsPage({ namespaces, rules }: { namespaces: Namespace[]; rules: NameRules }) {
  const [ns, setNs] = useState(namespaces[0]?.name ?? "");
  const [name, setName] = useState("");
  const [list, setList] = useState<Workload[] | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [formError, setFormError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    if (!ns) return;
    try {
      setList(await listWorkloads(ns));
      setError("");
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Could not load workloads.");
    }
  }, [ns]);

  useEffect(() => {
    setList(null);
    setOpen(null);
    setNotice("");
    load();
  }, [load]);

  if (namespaces.length === 0) {
    return <p>None of your business units has a ready namespace yet. Order one first.</p>;
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const problem = check(name, rules);
    if (problem) return setFormError(problem);
    setFormError("");
    setBusy(true);
    try {
      const w = await placeWorkload(ns, name);
      setName("");
      setNotice(`Ordered ${w.name}; the operator is creating its identity.`);
      await load();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Could not order the workload.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2>Workloads</h2>
      <label>
        Namespace
        <select value={ns} onChange={(e) => setNs(e.target.value)}>
          {namespaces.map((n) => (
            <option key={n.name} value={n.name}>
              {n.name} ({n.businessUnit})
            </option>
          ))}
        </select>
      </label>
      {open ? (
        <WorkloadDetail namespace={ns} name={open} onBack={() => setOpen(null)} />
      ) : (
        <>
          {error && <p role="alert">{error}</p>}
          {notice && <p role="status">{notice}</p>}
          {list === null ? (
            !error && <p>Loading…</p>
          ) : list.length === 0 ? (
            <p>No workloads in {ns} yet.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Status</th>
                  <th>SPIFFE ID</th>
                  <th>Hostname</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.map((w) => (
                  <tr key={w.name}>
                    <td>{w.name}</td>
                    <td>
                      {w.status}
                      {w.reason ? ` (${w.reason})` : ""}
                    </td>
                    <td>{w.spiffeId}</td>
                    <td>{w.hostname}</td>
                    <td>
                      <button onClick={() => setOpen(w.name)}>Details of {w.name}</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <form onSubmit={submit} aria-label="New workload" className="secret-form">
            <h3>New workload</h3>
            <label>
              Workload name
              <input value={name} onChange={(e) => setName(e.target.value)} maxLength={40} required autoComplete="off" spellCheck={false} />
            </label>
            <p className="hint">The operator creates a ServiceAccount with this name in {ns}; its SPIFFE ID and hostname follow from it.</p>
            <div className="actions">
              <button type="submit" disabled={busy}>
                {busy ? "Ordering…" : "Order workload"}
              </button>
              <button type="button" onClick={load}>
                Refresh
              </button>
            </div>
            {formError && <p role="alert">{formError}</p>}
          </form>
        </>
      )}
    </section>
  );
}
