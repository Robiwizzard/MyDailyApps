import { useEffect, useState } from "react";
import { api, AppView, FilesInfo } from "../api";
import { Modal } from "./Modal";

function size(n: number) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

/** What the app writes, a keep/temporary suggestion for each file, and the tools to check. */
export function FilesDialog({ app, onClose, onChanged }: { app: AppView; onClose: () => void; onChanged: () => void }) {
  const [info, setInfo] = useState<FilesInfo | null>(null);
  const [chosen, setChosen] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = (i: FilesInfo, resetChoice: boolean) => {
    setInfo(i);
    if (resetChoice) {
      const anySynced = i.files.some((f) => f.synced);
      // Start from the current cloud settings, or from the suggestions.
      setChosen(new Set(i.files.filter((f) => (anySynced ? f.synced : f.keep && !f.gone)).map((f) => f.key)));
    }
  };

  useEffect(() => {
    api.files(app.id).then((i) => load(i, true), (e) => setError((e as Error).message));
  }, [app.id]);

  const run = async (fn: () => Promise<void>) => {
    setError(null);
    setMsg(null);
    setBusy(true);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const toggle = (k: string) =>
    setChosen((s) => {
      const n = new Set(s);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });

  const record = (start: boolean) =>
    run(async () => {
      const r = await api.record(app.id, start);
      load(r.files, false);
      if (start) setMsg("Recording. Now save something in the app, then click Done.");
      else {
        const n = r.changed?.length ?? 0;
        setMsg(n === 0 ? "Nothing changed while recording. Did the app save?" : `${n} file${n === 1 ? "" : "s"} changed while you saved; marked as keep.`);
        if (r.changed) setChosen((s) => new Set([...s, ...r.changed!]));
      }
    });

  const aside = (key: string, action: "aside" | "restore" | "keep" | "temporary") =>
    run(async () => {
      load(await api.aside(app.id, key, action), false);
      if (action === "aside") setMsg("Set aside. Start the app and look whether your data is still there, then stop it and answer here.");
      if (action === "keep") setChosen((s) => new Set([...s, key]));
      if (action === "temporary")
        setChosen((s) => {
          const n = new Set(s);
          n.delete(key);
          return n;
        });
    });

  const apply = () =>
    run(async () => {
      load(await api.applyFiles(app.id, [...chosen]), true);
      onChanged();
      setMsg(
        chosen.size === 0
          ? "Cloud sync turned off for this app. Its files stay on this computer and in Drive."
          : info?.running
            ? "Saved. These files are synced when the app exits."
            : "Saved. Syncing them now (the card shows the result).",
      );
    });

  const files = info?.files ?? [];
  return (
    <Modal title={`${app.name}: files`} onClose={onClose} wide>
      <p className="muted small">
        What the app wrote while MyDailyApps watched it ({info?.runs ?? 0} run{info?.runs === 1 ? "" : "s"} so far). Tick what should be kept in your Google
        Drive. Suggestions come from names, folders and how files changed between runs.
      </p>
      <div className="btn-row">
        {info?.recording ? (
          <button type="button" className="btn btn-primary" onClick={() => record(false)} disabled={busy}>
            Done saving
          </button>
        ) : (
          <button
            type="button"
            className="btn btn-outline"
            onClick={() => record(true)}
            disabled={busy || !info?.running}
            title={info?.running ? "" : "Start the app first"}
          >
            Record a save
          </button>
        )}
        {!info?.running && <span className="muted small">Record a save works while the app runs.</span>}
      </div>
      {msg && (
        <p className="info-text" role="status">
          {msg}
        </p>
      )}
      {error && (
        <p className="error-text" role="alert">
          {error}
        </p>
      )}

      {info && files.length === 0 && (
        <div className="empty">
          <p className="muted">Nothing seen yet. Start the app, use it (save something), stop it, then come back here.</p>
        </div>
      )}
      {files.length > 0 && (
        <div className="files-table" role="table" aria-label="Files the app wrote">
          {files.map((f) => (
            <div key={f.key} className={`file-row${f.gone ? " file-gone" : ""}`} role="row">
              <label className="file-main" role="cell">
                <input type="checkbox" checked={chosen.has(f.key)} onChange={() => toggle(f.key)} disabled={f.aside} />
                <span className="file-path">
                  <code>{f.path}</code>
                  <span className="file-meta">
                    {f.where === "data" ? "data folder" : "app folder"} · {size(f.size)}
                    {f.sqlite ? " · SQLite" : ""}
                    {f.synced ? " · synced now" : ""}
                    {f.gone ? " · no longer there" : ""}
                  </span>
                </span>
              </label>
              <div className="file-why" role="cell">
                <span className={`chip-small ${f.keep ? "chip-keep" : "chip-temp"}`}>{f.keep ? "Keep" : "Temporary"}</span>
                <span className="muted small">{f.reasons.join(" · ")}</span>
              </div>
              <div className="file-actions" role="cell">
                {f.question ? (
                  <div className="file-question">
                    <span className="small">Without it, was your data still there?</span>
                    <button type="button" className="btn btn-ghost" onClick={() => aside(f.key, "temporary")} disabled={busy}>
                      Yes, all there
                    </button>
                    <button type="button" className="btn btn-ghost" onClick={() => aside(f.key, "keep")} disabled={busy}>
                      No, it was gone
                    </button>
                  </div>
                ) : f.aside ? (
                  <>
                    <span className="muted small">Set aside: start the app to test</span>
                    <button type="button" className="btn btn-ghost" onClick={() => aside(f.key, "restore")} disabled={busy}>
                      Put back
                    </button>
                  </>
                ) : (
                  !f.gone && (
                    <button
                      type="button"
                      className="btn btn-ghost"
                      onClick={() => aside(f.key, "aside")}
                      disabled={busy || info?.running}
                      title="Move the file away, run the app and see whether anything is missing"
                    >
                      Test without it
                    </button>
                  )
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="modal-actions">
        <button type="button" className="btn btn-quiet" onClick={onClose}>
          Close
        </button>
        <button type="button" className="btn btn-primary" onClick={apply} disabled={busy || !info}>
          Use ticked files for cloud sync
        </button>
      </div>
    </Modal>
  );
}
