import type { Status } from "../api";

const marks: Record<string, string> = {
  pending: "Waiting",
  running: "In progress",
  done: "Done",
  error: "Problem",
  skipped: "Skipped",
};

export function LoadingScreen({ status, error }: { status: Status | null; error: string | null }) {
  return (
    <main className="loading" aria-busy="true">
      <div className="loading-box">
        <div className="brand brand-lg">
          <span className="brand-mark" aria-hidden="true" />
          MyDailyApps
        </div>
        <p className="muted">Getting your library ready…</p>
        {error && <p className="error-text" role="alert">Cannot reach the host: {error}</p>}
        <ol className="steps" aria-live="polite">
          {(status?.steps ?? []).map((s) => (
            <li key={s.id} className={`step step-${s.state}`}>
              <span className="step-dot" aria-hidden="true" />
              <span className="step-text">
                <span className="step-label">{s.label}</span>
                <span className="step-state">
                  {marks[s.state]}
                  {s.detail ? ` · ${s.detail}` : ""}
                </span>
              </span>
            </li>
          ))}
        </ol>
      </div>
    </main>
  );
}
