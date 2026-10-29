import { useCallback, useEffect, useState } from "react";
import { ApiError, deleteSecret, listSecrets, type NameRules, type Namespace, type SecretMeta } from "./api";
import { SecretForm } from "./SecretForm";

type Mode = { kind: "none" } | { kind: "new" } | { kind: "replace"; secret: SecretMeta } | { kind: "delete"; secret: SecretMeta };

export function SecretsPage({ namespaces, rules }: { namespaces: Namespace[]; rules: NameRules }) {
  const [ns, setNs] = useState(namespaces[0]?.name ?? "");
  const [secrets, setSecrets] = useState<SecretMeta[] | null>(null);
  const [mode, setMode] = useState<Mode>({ kind: "none" });
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    if (!ns) return;
    try {
      setSecrets(await listSecrets(ns));
      setError("");
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Could not load secrets.");
    }
  }, [ns]);

  useEffect(() => {
    setSecrets(null);
    setMode({ kind: "none" });
    setNotice("");
    load();
  }, [load]);

  if (namespaces.length === 0) {
    return <p>None of your business units has a ready namespace yet. Order one first.</p>;
  }

  async function confirmDelete(s: SecretMeta) {
    setBusy(true);
    try {
      await deleteSecret(ns, s.name);
      setMode({ kind: "none" });
      setNotice(`Deleted ${s.name}.`);
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Could not delete the secret.");
    } finally {
      setBusy(false);
    }
  }

  const example = mode.kind === "replace" || mode.kind === "delete" ? mode.secret.name : (secrets?.[0]?.name ?? "<name>");

  return (
    <section>
      <h2>Secrets</h2>
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
      {error && <p role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {secrets === null ? (
        !error && <p>Loading…</p>
      ) : secrets.length === 0 ? (
        <p>No secrets in {ns} yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Keys</th>
              <th>Version</th>
              <th>Changed by</th>
              <th>Changed</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {secrets.map((s) => (
              <tr key={s.name}>
                <td>{s.name}</td>
                <td>{s.keys.join(", ")}</td>
                <td>{s.version}</td>
                <td>{s.updatedBy}</td>
                <td>{new Date(s.updatedAt).toLocaleString()}</td>
                <td>
                  <button onClick={() => setMode({ kind: "replace", secret: s })}>Replace values of {s.name}</button>{" "}
                  <button onClick={() => setMode({ kind: "delete", secret: s })}>Delete {s.name}</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {mode.kind === "none" && (
        <button
          onClick={() => {
            setNotice("");
            setMode({ kind: "new" });
          }}
        >
          New secret
        </button>
      )}
      {mode.kind === "delete" && (
        <div role="alertdialog" aria-label={`Delete ${mode.secret.name}`} className="confirm">
          <p>
            Delete <strong>{mode.secret.name}</strong> with all its versions? Pods using it keep their current values until
            they restart, then fail to start if they still reference it.
          </p>
          <button onClick={() => confirmDelete(mode.secret)} disabled={busy}>
            Yes, delete {mode.secret.name}
          </button>{" "}
          <button onClick={() => setMode({ kind: "none" })}>Cancel</button>
        </div>
      )}
      {(mode.kind === "new" || mode.kind === "replace") && (
        <SecretForm
          key={mode.kind === "replace" ? `r-${mode.secret.name}` : "new"}
          namespace={ns}
          rules={rules}
          existing={mode.kind === "replace" ? { name: mode.secret.name, keys: mode.secret.keys } : undefined}
          onCancel={() => setMode({ kind: "none" })}
          onSaved={(r) => {
            setMode({ kind: "none" });
            setNotice(`Saved ${r.name} (version ${r.version}, keys ${r.keys.join(", ")}).`);
            load();
          }}
        />
      )}
      <aside aria-label="How to use" className="howto">
        <h3>How to use</h3>
        <p>
          Each secret becomes a Kubernetes Secret of the same name in {ns}. Load all its keys as environment variables:
        </p>
        <pre>{`envFrom:\n  - secretRef:\n      name: ${example}`}</pre>
        <p>Environment variables are read when a container starts: after replacing values, restart the pods to pick them up.</p>
      </aside>
    </section>
  );
}
