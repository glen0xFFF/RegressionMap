import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "../api";
import { CopyIcon, KeyIcon } from "./Icons";
import type { ApiKey, CreatedKey, KeyScope, Me } from "../types";

const SCOPES: { id: KeyScope; label: string }[] = [
  { id: "analyze", label: "analyze — run analyses and verifications" },
  { id: "admin", label: "admin — everything, including key management" },
];

function fmt(iso: string | null): string {
  if (!iso) return "never";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

/** curl example for the new-key box. Uses a placeholder, never the real secret. */
export function curlExample(origin: string): string {
  return [
    `curl -X POST "${origin}/api/analyses" \\`,
    `  -H "Authorization: Bearer <YOUR_KEY>" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -d '{"repo":"<REPO>","baseline":"<BASELINE_SHA>","command_id":"node-test"}'`,
  ].join("\n");
}

function NewSecret({ created, onDone }: { created: CreatedKey; onDone: () => void }) {
  const [copy, setCopy] = useState<"idle" | "copied" | "failed">("idle");

  async function doCopy() {
    try {
      await navigator.clipboard.writeText(created.secret);
      setCopy("copied");
    } catch {
      setCopy("failed");
    }
  }

  return (
    <section className="card secret-card" data-testid="new-secret" aria-live="polite">
      <div className="card-title">
        <span className="accent">
          <KeyIcon />
        </span>
        <h3 className="card-h">
          New key <span className="muted">“{created.key.name}”</span>
        </h3>
        <span className={`pill scope-pill scope-${created.key.scope}`}>{created.key.scope}</span>
      </div>
      <p className="secret-warn">Copy this key now. You will not be able to see it again.</p>
      <div className="secret-box">
        <code className="secret-text">{created.secret}</code>
        <button type="button" className="btn-outline btn-sm" onClick={doCopy}>
          <CopyIcon size={13} /> {copy === "copied" ? "Copied" : "Copy"}
        </button>
      </div>
      {copy === "failed" && (
        <p className="error small" role="alert">
          Copy failed. Select the key and copy it manually.
        </p>
      )}
      <h4>Use it</h4>
      <pre className="output curl" data-testid="curl-example">
        {curlExample(window.location.origin)}
      </pre>
      <button type="button" className="btn-outline btn-sm" onClick={onDone}>
        Done, I saved it
      </button>
    </section>
  );
}

export function KeysPage({ me }: { me: Me | null }) {
  const [keys, setKeys] = useState<ApiKey[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [scope, setScope] = useState<KeyScope>("analyze");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  // Held only in this component's state: gone once the page unmounts.
  const [created, setCreated] = useState<CreatedKey | null>(null);
  const [confirmId, setConfirmId] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<string | null>(null);
  const [revokeError, setRevokeError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.keys().then(
      (r) => !cancelled && setKeys(r.keys ?? []),
      (e) => !cancelled && setLoadError(errorMessage(e)),
    );
    return () => {
      cancelled = true;
    };
  }, []);

  const trimmed = name.trim();
  const canCreate = !creating && trimmed.length > 0 && trimmed.length <= 64;

  async function create(e: FormEvent) {
    e.preventDefault();
    if (!canCreate) return;
    setCreating(true);
    setCreateError(null);
    try {
      const res = await api.createKey({ name: trimmed, scope });
      setCreated(res);
      setKeys((list) => [res.key, ...(list ?? []).filter((k) => k.id !== res.key.id)]);
      setName("");
    } catch (err) {
      setCreateError(errorMessage(err));
    } finally {
      setCreating(false);
    }
  }

  async function revoke(id: string) {
    setRevoking(id);
    setRevokeError(null);
    try {
      await api.revokeKey(id);
      setKeys((list) => (list ?? []).map((k) => (k.id === id ? { ...k, revoked: true } : k)));
    } catch (err) {
      setRevokeError(errorMessage(err));
    } finally {
      setConfirmId(null);
      setRevoking(null);
    }
  }

  const ownId = me?.mode === "key" ? me.key.id : null;

  return (
    <section className="middle wide" data-testid="keys-page">
      <div className="topbar">
        <h2 className="topbar-title">API Keys</h2>
        <span className="muted small">
          Keys authenticate requests that do not come from this machine. Secrets are shown once and stored only as a
          hash.
        </span>
      </div>
      <div className="content scroll keys-content">
        {created && <NewSecret created={created} onDone={() => setCreated(null)} />}

        <form className="card keys-create" onSubmit={create}>
          <h3 className="card-h">Create key</h3>
          <div className="keys-create-row">
            <label className="field">
              <span>Key name</span>
              <input
                type="text"
                value={name}
                maxLength={64}
                placeholder="ci, laptop, …"
                onChange={(e) => setName(e.target.value)}
                disabled={creating}
              />
            </label>
            <label className="field">
              <span>Scope</span>
              <select value={scope} onChange={(e) => setScope(e.target.value as KeyScope)} disabled={creating}>
                {SCOPES.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.label}
                  </option>
                ))}
              </select>
            </label>
            <button type="submit" className="btn-primary" disabled={!canCreate}>
              {creating ? "Creating…" : "Create"}
            </button>
          </div>
          {createError && (
            <p className="error" role="alert">
              {createError}
            </p>
          )}
        </form>

        {revokeError && (
          <div className="banner banner-error keys-banner" role="alert">
            {revokeError}
          </div>
        )}

        {loadError ? (
          <div className="banner banner-error keys-banner" role="alert">
            {loadError}
          </div>
        ) : !keys ? (
          <div className="empty-state">Loading keys…</div>
        ) : keys.length === 0 ? (
          <div className="empty-state">No API keys yet.</div>
        ) : (
          <div className="card keys-table-card">
            <table className="changes keys-table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Scope</th>
                  <th>Prefix</th>
                  <th>Created</th>
                  <th>Last used</th>
                  <th>Status</th>
                  <th>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <tr key={k.id} className={k.revoked ? "key-revoked" : undefined}>
                    <td>
                      <span className="key-name">{k.name}</span>
                      {k.id === ownId && <span className="tag key-own">this key</span>}
                    </td>
                    <td>
                      <span className={`pill scope-pill scope-${k.scope}`}>{k.scope}</span>
                    </td>
                    <td>
                      <code>{`${k.prefix}…`}</code>
                    </td>
                    <td className="muted small">{fmt(k.created_at)}</td>
                    <td className="muted small">{fmt(k.last_used_at)}</td>
                    <td>
                      {k.revoked ? (
                        <span className="key-status key-status-revoked">Revoked</span>
                      ) : (
                        <span className="key-status key-status-active">
                          <span className="dot dot-pass" aria-hidden="true" /> Active
                        </span>
                      )}
                    </td>
                    <td className="key-actions">
                      {!k.revoked &&
                        (confirmId === k.id ? (
                          <span className="revoke-confirm">
                            <span className="small">Revoke “{k.name}”? This is permanent.</span>
                            <button
                              type="button"
                              className="btn-danger btn-sm"
                              onClick={() => revoke(k.id)}
                              disabled={revoking === k.id}
                            >
                              {revoking === k.id ? "Revoking…" : "Confirm revoke"}
                            </button>
                            <button
                              type="button"
                              className="btn-outline btn-sm"
                              onClick={() => setConfirmId(null)}
                              disabled={revoking === k.id}
                            >
                              Cancel
                            </button>
                          </span>
                        ) : (
                          <button
                            type="button"
                            className="btn-outline btn-sm"
                            onClick={() => {
                              setRevokeError(null);
                              setConfirmId(k.id);
                            }}
                          >
                            Revoke
                          </button>
                        ))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  );
}
