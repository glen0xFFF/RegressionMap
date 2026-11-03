import { useEffect, useState } from "react";
import { api, errorMessage } from "../api";
import { buildVerifyRequest } from "../logic";
import { TestRunView } from "./TestRunView";
import { ShieldIcon } from "./Icons";
import type { Command, Verification, VerifyRequest } from "../types";

export function VerifyPanel({
  analysisId,
  commands,
  defaultCommandId,
  verifications,
  onVerified,
}: {
  analysisId: string;
  commands: Command[];
  defaultCommandId: string;
  verifications: Verification[];
  onVerified: (v: Verification) => void;
}) {
  const [commandId, setCommandId] = useState(defaultCommandId);
  const [approved, setApproved] = useState(false);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [last, setLast] = useState<Verification | null>(null);

  useEffect(() => setCommandId(defaultCommandId), [defaultCommandId]);

  const cmd = commands.find((c) => c.id === commandId);

  async function run() {
    setError(null);
    let body: VerifyRequest;
    try {
      body = buildVerifyRequest(commandId, approved);
    } catch (e) {
      setError(errorMessage(e));
      return;
    }
    setRunning(true);
    try {
      const v = await api.verify(analysisId, body);
      setLast(v);
      onVerified(v);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setRunning(false);
      setApproved(false); // every run requires fresh approval
    }
  }

  const history = [...verifications].reverse();
  const shown =
    last && !history.some((v) => v.at === last.at && v.command_id === last.command_id) ? [last, ...history] : history;

  return (
    <section className="card verify-card">
      <div className="card-title">
        <span className="accent">
          <ShieldIcon />
        </span>
        <h3 className="card-h">Verify fix</h3>
      </div>
      <p className="muted small">
        Runs the allowlisted command in the repository working tree (uncommitted changes included). Nothing runs
        without your explicit approval.
      </p>
      {commands.length === 0 ? (
        <p className="muted">No allowlisted commands available.</p>
      ) : (
        <label className="field">
          <span>Command</span>
          <select
            value={commandId}
            onChange={(e) => {
              setCommandId(e.target.value);
              setApproved(false);
            }}
            disabled={running}
          >
            {commands.map((c) => (
              <option key={c.id} value={c.id}>
                {c.id} — {c.description}
              </option>
            ))}
          </select>
        </label>
      )}
      <div className="argv">
        <span className="muted">Exact argv:</span>{" "}
        <code data-testid="verify-argv">{cmd ? JSON.stringify(cmd.argv) : "(unknown command)"}</code>
      </div>
      <label className="approve">
        <input
          type="checkbox"
          checked={approved}
          onChange={(e) => setApproved(e.target.checked)}
          disabled={running || !cmd}
        />
        I approve running this command
      </label>
      <button type="button" className="btn-primary" onClick={run} disabled={!approved || running || !cmd}>
        {running ? "Running…" : "Run verification"}
      </button>
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {shown.length > 0 ? (
        <div className="verifs">
          {shown.map((v, i) => (
            <div key={`${v.at}-${i}`} className="verif">
              <div className="muted small">
                {v.at} · <code>{JSON.stringify(v.argv)}</code>
              </div>
              <TestRunView title={`Verification (${v.command_id})`} run={v.run} />
            </div>
          ))}
        </div>
      ) : (
        <p className="muted small">No verification runs yet.</p>
      )}
    </section>
  );
}
