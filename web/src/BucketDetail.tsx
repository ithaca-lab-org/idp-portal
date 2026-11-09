import { useCallback, useEffect, useRef, useState } from "react";
import {
  ApiError,
  getBucket,
  listWorkloads,
  placeGrant,
  revokeGrant,
  type BucketDetail as Detail,
  type ResourcesInfo,
  type Workload,
} from "./api";
import { CopyButton } from "./WorkloadDetail";

/** One bucket: where it is, who may use it, and the form that gives a workload access. */
export function BucketDetail({ info, namespace, name, onBack }: { info: ResourcesInfo; namespace: string; name: string; onBack: () => void }) {
  const [b, setB] = useState<Detail | null>(null);
  const [workloads, setWorkloads] = useState<Workload[] | null>(null);
  const [workload, setWorkload] = useState("");
  const [access, setAccess] = useState<string>(info.access[0]?.value ?? "reader");
  const [error, setError] = useState("");
  const [formError, setFormError] = useState("");
  const [notice, setNotice] = useState("");
  const [confirming, setConfirming] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const latest = useRef(0); // only the latest request counts
  const load = useCallback(async () => {
    const id = ++latest.current;
    try {
      const r = await getBucket(namespace, name);
      if (id !== latest.current) return;
      setB(r);
      setError("");
    } catch (e) {
      if (id !== latest.current) return;
      setError(e instanceof ApiError ? e.message : "Could not load the bucket.");
    }
  }, [namespace, name]);

  useEffect(() => {
    setB(null);
    load();
    return () => {
      latest.current++;
    };
  }, [load]);

  // the workloads come from the Workloads page's API; when that page is off, none are offered
  useEffect(() => {
    let live = true;
    listWorkloads(namespace).then(
      (l) => live && (setWorkloads(l), setWorkload((w) => w || l[0]?.name || "")),
      () => live && setWorkloads([]),
    );
    return () => {
      live = false;
    };
  }, [namespace]);

  async function give(e: React.FormEvent) {
    e.preventDefault();
    if (!workload) return setFormError("Choose a workload.");
    setFormError("");
    setNotice("");
    setBusy(true);
    try {
      await placeGrant(namespace, workload, name, access);
      setNotice(`Giving ${workload} access to ${name}.`);
      await load();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Could not give access.");
    } finally {
      setBusy(false);
    }
  }

  async function revoke(grant: string) {
    setBusy(true);
    setNotice("");
    try {
      await revokeGrant(namespace, grant);
      setConfirming(null);
      setNotice("Access is being removed.");
      await load();
    } catch (err) {
      setConfirming(null);
      setError(err instanceof ApiError ? err.message : "Could not revoke the access.");
    } finally {
      setBusy(false);
    }
  }

  const label = (v: string) => info.access.find((a) => a.value === v)?.label ?? v;
  const uri = b?.bucketName ? `gs://${b.bucketName}` : "";
  return (
    <section aria-label={`Bucket ${name}`}>
      <button type="button" onClick={onBack}>
        Back to buckets
      </button>
      <h3>{name}</h3>
      {error && <p role="alert">{error}</p>}
      {!b ? (
        !error && <p>Loading…</p>
      ) : (
        <>
          <dl>
            <dt>Status</dt>
            <dd>
              {b.status}
              {b.reason ? ` (${b.reason})` : ""}
              {b.message ? `: ${b.message}` : ""}
            </dd>
            <dt>Namespace</dt>
            <dd>{b.namespace}</dd>
            <dt>Bucket</dt>
            <dd>
              {uri ? (
                <>
                  <code>{uri}</code> <CopyButton label="bucket URI" value={uri} />
                </>
              ) : (
                "Not created yet."
              )}
            </dd>
          </dl>
          <h4>Access</h4>
          {notice && <p role="status">{notice}</p>}
          {b.grants.length === 0 ? (
            <p>No workload has access to this bucket yet.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Workload</th>
                  <th>Access</th>
                  <th>Status</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {b.grants.map((g) => (
                  <tr key={g.name}>
                    <td>{g.workload}</td>
                    <td>{label(g.access)}</td>
                    <td>
                      {g.status}
                      {g.reason ? ` (${g.reason})` : ""}
                      {g.message ? `: ${g.message}` : ""}
                    </td>
                    <td>
                      {confirming === g.name ? (
                        <>
                          <span>Remove the access of {g.workload}?</span>{" "}
                          <button type="button" disabled={busy} onClick={() => revoke(g.name)}>
                            Confirm revoke of {g.workload}
                          </button>{" "}
                          <button type="button" onClick={() => setConfirming(null)}>
                            Cancel
                          </button>
                        </>
                      ) : (
                        <button type="button" onClick={() => setConfirming(g.name)}>
                          Revoke access of {g.workload}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <form onSubmit={give} aria-label="Give a workload access" className="secret-form">
            <h4>Give a workload access</h4>
            {workloads !== null && workloads.length === 0 ? (
              <p>There are no workloads in {namespace}. Order one on the Workloads page first.</p>
            ) : (
              <>
                <label>
                  Workload
                  <select value={workload} onChange={(e) => setWorkload(e.target.value)} disabled={workloads === null}>
                    {(workloads ?? []).map((w) => (
                      <option key={w.name} value={w.name}>
                        {w.name}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Access
                  <select value={access} onChange={(e) => setAccess(e.target.value)}>
                    {info.access.map((a) => (
                      <option key={a.value} value={a.value}>
                        {a.label}
                      </option>
                    ))}
                  </select>
                </label>
                <p className="hint">
                  The workload reaches the bucket with its own ServiceAccount, no key needed. At most {info.maxGrants} grants per namespace. To
                  change the access, revoke it and give it again.
                </p>
                <div className="actions">
                  <button type="submit" disabled={busy || workloads === null}>
                    {busy ? "Working…" : "Give access"}
                  </button>
                  <button type="button" onClick={load}>
                    Refresh
                  </button>
                </div>
              </>
            )}
            {formError && <p role="alert">{formError}</p>}
          </form>
        </>
      )}
    </section>
  );
}
