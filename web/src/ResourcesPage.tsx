import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, listBuckets, placeBucket, type Bucket, type ResourcesInfo } from "./api";
import { BucketDetail } from "./BucketDetail";
import { pickNs } from "./namespaces";

const NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

// the same rules as the API, the BucketClaim CRD and the broker; the limits come from the API
function check(name: string, max: number): string {
  if (name.length > max || !NAME_RE.test(name)) return `Name must be a lowercase DNS label of at most ${max} characters (a-z, 0-9, -).`;
  if (name.startsWith("goog") || name.includes("google")) return 'Name cannot contain "google" or start with "goog".';
  return "";
}

export function ResourcesPage({ info, active, ns: wanted, onNs }: { info: ResourcesInfo; active: boolean; ns: string; onNs: (ns: string) => void }) {
  const { namespaces, maxNamespaceLength: maxNs, maxNameLength: maxName } = info;
  const usable = namespaces.filter((n) => n.name.length <= maxNs && !n.name.startsWith("goog") && !n.name.includes("google"));
  const ns = pickNs(usable.map((n) => n.name), wanted);
  const [name, setName] = useState("");
  const [list, setList] = useState<Bucket[] | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [formError, setFormError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  // only the latest request counts: a slow answer for a namespace we left (or an older request) is ignored
  const latest = useRef(0);
  const load = useCallback(async () => {
    if (!ns) return;
    const id = ++latest.current;
    try {
      const r = await listBuckets(ns);
      if (id !== latest.current) return;
      setList(r);
      setError("");
    } catch (e) {
      if (id !== latest.current) return;
      setError(e instanceof ApiError ? e.message : "Could not load buckets.");
    }
  }, [ns]);

  // a new namespace starts from "Loading…"; coming back to the page reloads silently
  useEffect(() => {
    latest.current++;
    setList(null);
    setOpen(null);
    setNotice("");
    return () => {
      latest.current++;
    };
  }, [ns]);

  useEffect(() => {
    if (active) load();
  }, [load, active]);

  if (namespaces.length === 0) {
    return <p>None of your business units has a ready namespace yet. Order one first.</p>;
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const problem = check(name, maxName);
    if (problem) return setFormError(problem);
    setFormError("");
    setBusy(true);
    try {
      const b = await placeBucket(ns, name);
      setName("");
      setNotice(`Ordered ${b.name}; the platform is creating the bucket.`);
      await load();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Could not order the bucket.");
    } finally {
      setBusy(false);
    }
  }

  const skipped = namespaces.length - usable.length;
  return (
    <section>
      <h2>Resources</h2>
      <label>
        Namespace
        <select value={ns} onChange={(e) => onNs(e.target.value)}>
          {namespaces.map((n) => {
            const ok = usable.includes(n);
            return (
              <option key={n.name} value={n.name} disabled={!ok}>
                {n.name} ({n.businessUnit}){ok ? "" : " - name too long for buckets"}
              </option>
            );
          })}
        </select>
      </label>
      {skipped > 0 && (
        <p className="hint">
          A bucket name is idp-&lt;namespace&gt;-&lt;name&gt;-&lt;hash&gt; and is at most 63 characters, so a namespace can own buckets only if its
          name has at most {maxNs} characters (and no &quot;goog&quot;).
        </p>
      )}
      {usable.length === 0 ? (
        <p>None of your namespaces can own buckets.</p>
      ) : open ? (
        <BucketDetail info={info} namespace={ns} name={open} onBack={() => { setOpen(null); load(); }} />
      ) : (
        <>
          {error && <p role="alert">{error}</p>}
          {notice && <p role="status">{notice}</p>}
          {list === null ? (
            !error && <p>Loading…</p>
          ) : list.length === 0 ? (
            <p>No buckets in {ns} yet.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Status</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.map((b) => (
                  <tr key={b.name}>
                    <td>{b.name}</td>
                    <td>
                      {b.status}
                      {b.reason ? ` (${b.reason})` : ""}
                    </td>
                    <td>
                      <button onClick={() => setOpen(b.name)}>Details of {b.name}</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <form onSubmit={submit} aria-label="New bucket" className="secret-form">
            <h3>New bucket</h3>
            <label>
              Bucket name
              <input value={name} onChange={(e) => setName(e.target.value)} maxLength={maxName} required autoComplete="off" spellCheck={false} />
            </label>
            <p className="hint">
              Creates a private Cloud Storage bucket for {ns}. At most {info.maxBuckets} buckets per namespace. Buckets cannot be deleted here, so
              pick a name you will keep.
            </p>
            <div className="actions">
              <button type="submit" disabled={busy}>
                {busy ? "Ordering…" : "Order bucket"}
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
