import { useState, type FormEvent } from "react";
import { ApiError, api, errorMessage } from "../api";
import { KeyIcon } from "./Icons";
import type { Me } from "../types";

/**
 * Sign-in card shown when the backend requires an API key. The key is
 * validated with GET /api/me before the caller stores it. It is never
 * logged, never put in a URL and never rendered back after entry.
 */
export function SignIn({ onSignedIn }: { onSignedIn: (me: Me, key: string) => void }) {
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const key = value.trim();
    if (!key || busy) return;
    setBusy(true);
    setError(null);
    try {
      const me = await api.me(key);
      setValue("");
      onSignedIn(me, key);
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 401 ? "Invalid or revoked API key." : errorMessage(err),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="content scroll signin-wrap">
      <form className="card signin-card" onSubmit={submit} aria-labelledby="signin-h">
        <div className="card-title">
          <span className="accent">
            <KeyIcon />
          </span>
          <h2 className="card-h" id="signin-h">
            Sign in with an API key
          </h2>
        </div>
        <p className="muted small">
          This RegressionMap server is not on your machine, so it needs an API key. Ask an admin for one, or create
          one with <code>server keys create</code>. The key stays in this browser tab only.
        </p>
        <label className="field">
          <span>API key</span>
          <input
            type="password"
            autoComplete="off"
            spellCheck={false}
            placeholder="rm_live_…"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            disabled={busy}
          />
        </label>
        <button type="submit" className="btn-primary" disabled={busy || !value.trim()}>
          {busy ? "Checking…" : "Sign in"}
        </button>
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
      </form>
    </div>
  );
}
