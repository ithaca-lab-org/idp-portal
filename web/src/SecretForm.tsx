import { useState } from "react";
import { ApiError, putSecret, type NameRules, type SecretResult } from "./api";

const NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const KEY_RE = /^[A-Za-z_][A-Za-z0-9_]{0,63}$/;
const MAX_KEYS = 20;
const MAX_VALUE = 8192;
const MAX_TOTAL = 64 * 1024;

type Row = { id: number; key: string; value: string };

let nextId = 1;
const row = (key = ""): Row => ({ id: nextId++, key, value: "" });
const bytes = (s: string) => new TextEncoder().encode(s).length;

function check(name: string, rows: Row[], rules: NameRules): string {
  if (name.length > 40 || !NAME_RE.test(name)) return "Name must be a lowercase DNS label of at most 40 characters (a-z, 0-9, -).";
  if (rules.reservedNames.includes(name)) return `The name ${name} is reserved.`;
  const prefix = rules.reservedPrefixes.find((p) => name.startsWith(p));
  if (prefix) return `Names starting with ${prefix} are reserved.`;
  if (rows.length < 1 || rows.length > MAX_KEYS) return `A secret needs 1 to ${MAX_KEYS} keys.`;
  const seen = new Set<string>();
  let total = 0;
  for (const r of rows) {
    if (!KEY_RE.test(r.key)) return "Key names start with a letter or _ and contain only letters, digits and _ (at most 64).";
    if (seen.has(r.key)) return `Key ${r.key} appears twice.`;
    seen.add(r.key);
    const n = bytes(r.value);
    if (n < 1 || n > MAX_VALUE) return `The value of ${r.key} must be 1 to ${MAX_VALUE} bytes.`;
    total += bytes(r.key) + n;
  }
  if (total > MAX_TOTAL) return "The secret is larger than 64 KB.";
  return "";
}

/**
 * Creates a secret or replaces all its values. Values are write-only: the inputs are masked, and
 * on success every value is dropped from state before the parent is told about the result.
 */
export function SecretForm({
  namespace,
  rules,
  existing,
  onSaved,
  onCancel,
}: {
  namespace: string;
  rules: NameRules;
  existing?: { name: string; keys: string[] };
  onSaved: (r: SecretResult) => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [rows, setRows] = useState<Row[]>(() => (existing ? existing.keys.map((k) => row(k)) : [row()]));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const update = (id: number, patch: Partial<Row>) => setRows((rs) => rs.map((r) => (r.id === id ? { ...r, ...patch } : r)));

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const problem = check(name, rows, rules);
    if (problem) return setError(problem);
    setError("");
    setBusy(true);
    try {
      const res = await putSecret(namespace, name, Object.fromEntries(rows.map((r) => [r.key, r.value])), !existing);
      setRows((rs) => rs.map((r) => ({ ...r, value: "" })));
      onSaved(res);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not save the secret.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} aria-label={existing ? `Replace values of ${existing.name}` : "New secret"} className="secret-form">
      <h3>{existing ? `Replace values of ${existing.name}` : "New secret"}</h3>
      {!existing && (
        <label>
          Secret name
          <input value={name} onChange={(e) => setName(e.target.value)} maxLength={40} required />
        </label>
      )}
      <p className="hint">Saving replaces every key of the secret. Values are never shown again.</p>
      {rows.map((r, i) => (
        <fieldset key={r.id} className="kv-row">
          <label>
            Key {i + 1}
            <input value={r.key} onChange={(e) => update(r.id, { key: e.target.value })} maxLength={64} autoComplete="off" spellCheck={false} />
          </label>
          <label>
            Value {i + 1}
            <input
              type="password"
              value={r.value}
              onChange={(e) => update(r.id, { value: e.target.value })}
              autoComplete="new-password"
              spellCheck={false}
            />
          </label>
          <button type="button" onClick={() => setRows((rs) => rs.filter((x) => x.id !== r.id))} disabled={rows.length === 1}>
            Remove key {i + 1}
          </button>
        </fieldset>
      ))}
      <div className="actions">
        <button type="button" onClick={() => setRows((rs) => [...rs, row()])} disabled={rows.length >= MAX_KEYS}>
          Add key
        </button>
        <button type="submit" disabled={busy}>
          {busy ? "Saving…" : "Save secret"}
        </button>
        <button type="button" onClick={onCancel}>
          Cancel
        </button>
      </div>
      {error && <p role="alert">{error}</p>}
    </form>
  );
}
