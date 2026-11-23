import { useCallback, useEffect, useRef, useState } from "react";
import {
  ApiError,
  listPipelines,
  listRuns,
  listWorkloads,
  placePipeline,
  placeRun,
  type Phase,
  type Pipeline,
  type PipelinesInfo,
  type Run,
  type Workload,
} from "./api";
import { pickNs } from "./namespaces";

const POLL_MS = 5000;
const REPO_RE = /^https:\/\/github\.com\/([A-Za-z0-9][A-Za-z0-9-]{0,38})\/([A-Za-z0-9._-]{1,100})$/;
const PATH_RE = /^[A-Za-z0-9._][A-Za-z0-9._/-]*$/;
const SHA_RE = /^[0-9a-f]{40}$/;

// the same rules as the API and the CRDs; the owner allowlist comes from the API
function checkRepo(repo: string, orgs: string[]): string {
  const m = REPO_RE.exec(repo);
  if (!m || m[2] === "." || m[2] === ".." || m[2].endsWith(".git"))
    return "Repository must look like https://github.com/<org>/<repo> (no trailing slash, no .git).";
  if (!orgs.some((o) => o.toLowerCase() === m[1].toLowerCase())) return `The owner ${m[1]} is not allowed; allowed: ${orgs.join(", ")}.`;
  return "";
}

function checkPath(path: string): string {
  if (path === "") return "";
  if (path.length > 128 || !PATH_RE.test(path) || path.includes("..") || path.includes("//") || path.endsWith("/"))
    return "Path must be a relative directory without '..', '//' or a trailing '/'.";
  return "";
}

function checkRef(ref: string): string {
  return SHA_RE.test(ref)
    ? ""
    : "Enter the full 40-character lowercase commit SHA. Deploy by commit (no tag, branch or short SHA): the image digest is pinned in the repository manifests.";
}

export function Badge({ phase }: { phase: Phase }) {
  return <span className={`badge badge-${phase.toLowerCase()}`}>{phase}</span>;
}

const pipelineBusy = (l: Pipeline[] | null) => !!l && l.some((p) => p.phase === "PENDING" || p.phase === "BUILDING");
const runsBusy = (l: Run[] | null) => !!l && l.some((r) => r.phase === "PENDING" || r.phase === "RUNNING");

/** Calls load every 5 s while busy is true; nothing runs otherwise (no SSE, no idle polling). */
function usePolling(busy: boolean, active: boolean, load: () => void) {
  useEffect(() => {
    if (!busy || !active) return;
    const t = setInterval(load, POLL_MS);
    return () => clearInterval(t);
  }, [busy, active, load]);
}

function PipelineCard({ ns, p, active }: { ns: string; p: Pipeline; active: boolean }) {
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState("");
  const [ref, setRef] = useState("");
  const [formError, setFormError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  const latest = useRef(0);
  const load = useCallback(async () => {
    const id = ++latest.current;
    try {
      const r = await listRuns(ns, p.name);
      if (id !== latest.current) return;
      setRuns(r);
      setError("");
    } catch (e) {
      if (id !== latest.current) return;
      setError(e instanceof ApiError ? e.message : "Could not load the runs.");
    }
  }, [ns, p.name]);

  useEffect(() => {
    if (active) load();
    return () => {
      latest.current++;
    };
  }, [load, active]);
  usePolling(runsBusy(runs), active, load);

  async function deploy(e: React.FormEvent) {
    e.preventDefault();
    const sha = ref.trim();
    const problem = checkRef(sha);
    if (problem) return setFormError(problem);
    setFormError("");
    setBusy(true);
    try {
      await placeRun(ns, p.name, sha);
      setRef("");
      setNotice(`Deploy of ${sha.slice(0, 7)} requested.`);
      await load();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Could not request the deploy.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="pipeline" aria-label={`Pipeline ${p.name}`}>
      <h3>
        {p.name} <Badge phase={p.phase} />
      </h3>
      {p.message && <p>{p.message}</p>}
      <dl>
        <dt>Repository</dt>
        <dd>
          {p.repo} ({p.path})
        </dd>
        {p.applicationName && (
          <>
            <dt>Application</dt>
            <dd>{p.applicationName}</dd>
          </>
        )}
      </dl>
      <form onSubmit={deploy} aria-label={`Deploy a version of ${p.name}`}>
        <h4>Deploy a version</h4>
        <label>
          Commit SHA for {p.name}
          <input value={ref} onChange={(e) => setRef(e.target.value)} maxLength={64} required autoComplete="off" spellCheck={false} />
        </label>
        <div className="actions">
          <button type="submit" disabled={busy || p.phase !== "READY"}>
            {busy ? "Requesting…" : `Deploy ${p.name}`}
          </button>
        </div>
        {p.phase !== "READY" && <p className="hint">You can deploy once the pipeline is READY.</p>}
        {formError && <p role="alert">{formError}</p>}
        {notice && <p role="status">{notice}</p>}
      </form>
      {error && <p role="alert">{error}</p>}
      {runs === null ? (
        !error && <p>Loading…</p>
      ) : runs.length === 0 ? (
        <p>No deploys yet.</p>
      ) : (
        <table aria-label={`Runs of ${p.name}`}>
          <thead>
            <tr>
              <th>Status</th>
              <th>Commit</th>
              <th>Requested by</th>
              <th>Time</th>
              <th>Message</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((r) => (
              <tr key={r.name}>
                <td>
                  <Badge phase={r.phase} />
                </td>
                <td title={r.ref}>{r.ref.slice(0, 7)}</td>
                <td>{r.requestedBy}</td>
                <td>{r.createdAt ? new Date(r.createdAt).toLocaleString() : ""}</td>
                <td>{r.message}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

export function PipelinesPage({
  info,
  active,
  ns: wanted,
  onNs,
}: {
  info: PipelinesInfo;
  active: boolean;
  ns: string;
  onNs: (ns: string) => void;
}) {
  const namespaces = info.namespaces;
  const ns = pickNs(namespaces.map((n) => n.name), wanted);
  const [list, setList] = useState<Pipeline[] | null>(null);
  const [workloads, setWorkloads] = useState<Workload[]>([]);
  const [workload, setWorkload] = useState("");
  const [repo, setRepo] = useState("");
  const [path, setPath] = useState("");
  const [error, setError] = useState("");
  const [formError, setFormError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  // only the latest request counts: a slow answer for a namespace we left is ignored
  const latest = useRef(0);
  const load = useCallback(async () => {
    if (!ns) return;
    const id = ++latest.current;
    try {
      const r = await listPipelines(ns);
      if (id !== latest.current) return;
      setList(r);
      setError("");
    } catch (e) {
      if (id !== latest.current) return;
      setError(e instanceof ApiError ? e.message : "Could not load pipelines.");
    }
  }, [ns]);

  const loadWorkloads = useCallback(async () => {
    if (!ns) return;
    const forNs = ns;
    try {
      const w = await listWorkloads(forNs);
      if (forNs === ns) setWorkloads(w);
    } catch {
      setWorkloads([]); // the form then has nothing to offer; the pipelines list still works
    }
  }, [ns]);

  useEffect(() => {
    latest.current++;
    setList(null);
    setWorkloads([]);
    setWorkload("");
    setNotice("");
    return () => {
      latest.current++;
    };
  }, [ns]);

  useEffect(() => {
    if (active) {
      load();
      loadWorkloads();
    }
  }, [load, loadWorkloads, active]);
  usePolling(pipelineBusy(list), active, load);

  if (namespaces.length === 0) {
    return <p>None of your business units has a ready namespace yet. Order one first.</p>;
  }

  const taken = new Set((list ?? []).map((p) => p.name));
  const choices = workloads.filter((w) => !taken.has(w.name));
  const chosen = choices.some((w) => w.name === workload) ? workload : (choices[0]?.name ?? "");

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const r = repo.trim();
    const p = path.trim();
    const problem = !chosen ? "Choose a workload." : checkRepo(r, info.allowedOrgs) || checkPath(p);
    if (problem) return setFormError(problem);
    setFormError("");
    setBusy(true);
    try {
      const created = await placePipeline(ns, chosen, r, p || undefined);
      setRepo("");
      setPath("");
      setNotice(`Ordered the pipeline for ${created.name}; the delivery controller is preparing it.`);
      await load();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Could not order the pipeline.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2>Pipelines</h2>
      <label>
        Namespace
        <select value={ns} onChange={(e) => onNs(e.target.value)}>
          {namespaces.map((n) => (
            <option key={n.name} value={n.name}>
              {n.name} ({n.businessUnit})
            </option>
          ))}
        </select>
      </label>
      {error && <p role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {list === null ? (
        !error && <p>Loading…</p>
      ) : list.length === 0 ? (
        <p>No pipelines in {ns} yet.</p>
      ) : (
        list.map((p) => <PipelineCard key={`${ns}/${p.name}`} ns={ns} p={p} active={active} />)
      )}
      <form onSubmit={submit} aria-label="New pipeline" className="secret-form">
        <h3>New pipeline</h3>
        <label>
          Workload
          <select value={chosen} onChange={(e) => setWorkload(e.target.value)} disabled={choices.length === 0}>
            {choices.length === 0 && <option value="">No workload without a pipeline</option>}
            {choices.map((w) => (
              <option key={w.name} value={w.name}>
                {w.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          Repository
          <input
            value={repo}
            onChange={(e) => setRepo(e.target.value)}
            placeholder={`https://github.com/${info.allowedOrgs[0] ?? "<org>"}/<repo>`}
            maxLength={200}
            required
            autoComplete="off"
            spellCheck={false}
          />
        </label>
        <label>
          Path (optional)
          <input value={path} onChange={(e) => setPath(e.target.value)} placeholder={info.defaultPath} maxLength={128} autoComplete="off" spellCheck={false} />
        </label>
        <p className="hint">
          The Kustomize directory of the repository ({info.defaultPath} by default). Allowed owners: {info.allowedOrgs.join(", ")}. A version is deployed by its
          full commit SHA; the image digest is pinned in the repository manifests.
        </p>
        <div className="actions">
          <button type="submit" disabled={busy}>
            {busy ? "Ordering…" : "Order pipeline"}
          </button>
          <button type="button" onClick={load}>
            Refresh
          </button>
        </div>
        {formError && <p role="alert">{formError}</p>}
      </form>
    </section>
  );
}
