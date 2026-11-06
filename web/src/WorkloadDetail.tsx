import { useEffect, useState } from "react";
import { ApiError, getWorkload, type Workload } from "./api";

/** A copy button; the clipboard can be refused (no permission, insecure context), so say so. */
export function CopyButton({ label, value }: { label: string; value: string }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setState("copied");
    } catch {
      setState("failed");
    }
  }
  return (
    <>
      <button type="button" onClick={copy} disabled={!value}>
        Copy {label}
      </button>
      {state === "copied" && <span role="status"> Copied.</span>}
      {state === "failed" && <span role="alert"> Could not copy; select the text and copy it.</span>}
    </>
  );
}

/** One workload: the identity the operator issued and the conditions it reported. */
export function WorkloadDetail({ namespace, name, onBack }: { namespace: string; name: string; onBack: () => void }) {
  const [w, setW] = useState<Workload | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    setW(null);
    getWorkload(namespace, name).then(
      (r) => live && (setW(r), setError("")),
      (e) => live && setError(e instanceof ApiError ? e.message : "Could not load the workload."),
    );
    return () => {
      live = false;
    };
  }, [namespace, name]);

  return (
    <section aria-label={`Workload ${name}`}>
      <button type="button" onClick={onBack}>
        Back to workloads
      </button>
      <h3>{name}</h3>
      {error && <p role="alert">{error}</p>}
      {!w ? (
        !error && <p>Loading…</p>
      ) : (
        <>
          <dl>
            <dt>Status</dt>
            <dd>
              {w.status}
              {w.reason ? ` (${w.reason})` : ""}
            </dd>
            <dt>Namespace</dt>
            <dd>{w.namespace}</dd>
            <dt>ServiceAccount</dt>
            <dd>{w.serviceAccount}</dd>
            <dt>SPIFFE ID</dt>
            <dd>
              {w.spiffeId ? <code>{w.spiffeId}</code> : "Not issued yet."} <CopyButton label="SPIFFE ID" value={w.spiffeId} />
            </dd>
            <dt>Hostname</dt>
            <dd>
              {w.hostname ? <code>{w.hostname}</code> : "Not reserved yet."} <CopyButton label="hostname" value={w.hostname} />
            </dd>
          </dl>
          <h4>Conditions</h4>
          {w.conditions.length === 0 ? (
            <p>The operator has not reported any conditions yet.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Type</th>
                  <th>Status</th>
                  <th>Reason</th>
                  <th>Message</th>
                  <th>Since</th>
                </tr>
              </thead>
              <tbody>
                {w.conditions.map((c) => (
                  <tr key={c.type}>
                    <td>{c.type}</td>
                    <td>{c.status}</td>
                    <td>{c.reason}</td>
                    <td>{c.message}</td>
                    <td>{c.lastTransitionTime ? new Date(c.lastTransitionTime).toLocaleString() : ""}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </section>
  );
}
