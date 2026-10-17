import { FormEvent, useState } from "react";
import { api, ApiError, CloudSpec, Finding, InstallOption, LibraryEntry } from "../api";
import { Modal } from "./Modal";

interface Props {
  mode: "add" | "edit";
  initial?: LibraryEntry; // edit: the app's current configuration
  groups: string[];
  onClose: () => void;
  onSaved: () => void;
}

const lines = (s: string) =>
  s
    .split("\n")
    .map((x) => x.trim())
    .filter(Boolean);
const csv = (s: string) =>
  s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);

function Notes({ findings, field }: { findings: Finding[]; field: string | string[] }) {
  const fields = Array.isArray(field) ? field : [field];
  const list = findings.filter((f) => fields.includes(f.field));
  if (list.length === 0) return null;
  return (
    <ul className="findings">
      {list.map((f, i) => (
        <li key={i} className={`finding finding-${f.confidence}`}>
          <span className="finding-tag">{f.confidence === "detected" ? "Detected" : f.confidence === "guessed" ? "Guessed" : "Missing"}</span>
          {f.note}
        </li>
      ))}
    </ul>
  );
}

/** Add an app from a repository link (detected, then reviewed), or edit one. */
export function AppForm({ mode, initial, groups, onClose, onSaved }: Props) {
  const [repo, setRepo] = useState(initial?.repo ?? "");
  const [token, setToken] = useState("");
  const [needToken, setNeedToken] = useState(false);
  const [entry, setEntry] = useState<LibraryEntry | null>(mode === "edit" && initial ? initial : null);
  const [findings, setFindings] = useState<Finding[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const set = (patch: Partial<LibraryEntry>) => setEntry((e) => (e ? { ...e, ...patch } : e));
  const setOpt = (i: number, patch: Partial<InstallOption>) =>
    set({ install: (entry?.install ?? []).map((o, j) => (j === i ? { ...o, ...patch } : o)) });
  const setCloud = (patch: Partial<CloudSpec>) => set({ cloud: { include: [], ...(entry?.cloud ?? {}), ...patch } });

  const runDetect = async () => {
    setError(null);
    setBusy(true);
    try {
      const d = await api.detect(repo, mode === "edit" ? initial?.id : undefined, token || undefined);
      // When editing, the name and group stay as they are: their notes would only confuse.
      setFindings(mode === "edit" ? d.findings.filter((f) => f.field !== "name" && !(f.field === "group" && initial?.group)) : d.findings);
      if (mode === "edit" && initial) {
        // Keep the identity; take the detected technical details.
        setEntry({ ...d.app, id: initial.id, name: initial.name || d.app.name, group: initial.group || d.app.group });
      } else {
        setEntry(d.app);
      }
      setNeedToken(false);
    } catch (e) {
      const err = e as ApiError;
      if (err.needsToken) setNeedToken(true);
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const save = async (ev: FormEvent) => {
    ev.preventDefault();
    if (!entry) return;
    setError(null);
    setBusy(true);
    const e: LibraryEntry = { ...entry, repo: repo.trim() || entry.repo };
    if (!e.port) delete e.port;
    if (!e.path) delete e.path;
    if (e.cloud && e.cloud.include.length === 0) e.cloud = null;
    try {
      if (mode === "add") await api.addApp(e);
      else await api.saveConfig(initial!.id, e);
      onSaved();
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };

  const title = mode === "add" ? "Add app" : `Edit ${initial?.name || initial?.id}`;
  return (
    <Modal title={title} onClose={onClose} wide>
      <form className="form" onSubmit={save}>
        <div className="detect-row">
          <label className="field grow">
            <span>Repository link</span>
            <input
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              placeholder="https://github.com/owner/app"
              autoComplete="off"
              spellCheck={false}
            />
          </label>
          <button type="button" className="btn btn-outline" onClick={runDetect} disabled={busy || (!repo.trim() && mode === "add")}>
            {busy && !entry ? "Reading…" : entry ? "Detect again" : "Detect"}
          </button>
        </div>
        {needToken && (
          <label className="field">
            <span>GitHub token (for a private repository; kept for this session only)</span>
            <input type="password" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" />
          </label>
        )}
        {mode === "add" && !entry && (
          <p className="muted small">
            MyDailyApps reads the repository (without running or installing anything) and drafts the settings. You review them before saving.
          </p>
        )}
        {error && (
          <p className="error-text" role="alert">
            {error}
          </p>
        )}
        <Notes findings={findings} field="all" />

        {entry && (
          <>
            <fieldset className="form-section">
              <legend>App</legend>
              <div className="form-grid">
                <label className="field">
                  <span>Name on the card</span>
                  <input value={entry.name ?? ""} onChange={(e) => set({ name: e.target.value })} required />
                </label>
                <label className="field">
                  <span>Group</span>
                  <input value={entry.group ?? ""} onChange={(e) => set({ group: e.target.value })} list="group-list" />
                  <datalist id="group-list">
                    {groups.map((g) => (
                      <option key={g} value={g} />
                    ))}
                  </datalist>
                </label>
                <label className="field">
                  <span>Id (folder name; cannot change later)</span>
                  <input value={entry.id} onChange={(e) => set({ id: e.target.value })} disabled={mode === "edit"} required />
                </label>
                <label className="field">
                  <span>Description</span>
                  <input value={entry.description ?? ""} onChange={(e) => set({ description: e.target.value })} />
                </label>
              </div>
              <Notes findings={findings} field={["name", "group"]} />
            </fieldset>

            <fieldset className="form-section">
              <legend>Install and start</legend>
              <p className="muted small">Options are tried in order; the first that works on this computer is used.</p>
              {(entry.install ?? []).map((o, i) => (
                <div key={i} className="install-row">
                  <div className="form-grid">
                    <label className="field">
                      <span>Type</span>
                      <select value={o.type} onChange={(e) => setOpt(i, { type: e.target.value as InstallOption["type"] })}>
                        <option value="release">release (download a package)</option>
                        <option value="git">git (clone and set up)</option>
                      </select>
                    </label>
                    <label className="field">
                      <span>Start command</span>
                      <input value={o.launch} onChange={(e) => setOpt(i, { launch: e.target.value })} required />
                    </label>
                    {o.type === "git" && (
                      <>
                        <label className="field">
                          <span>Setup command (run once after cloning)</span>
                          <input value={o.setup ?? ""} onChange={(e) => setOpt(i, { setup: e.target.value })} />
                        </label>
                        <label className="field">
                          <span>Needs these tools (comma separated)</span>
                          <input value={(o.requires ?? []).join(", ")} onChange={(e) => setOpt(i, { requires: csv(e.target.value) })} />
                        </label>
                      </>
                    )}
                    {o.type === "release" && (
                      <label className="field">
                        <span>Asset name must contain (optional)</span>
                        <input value={o.asset ?? ""} onChange={(e) => setOpt(i, { asset: e.target.value })} />
                      </label>
                    )}
                  </div>
                  <div className="btn-row">
                    {i > 0 && (
                      <button
                        type="button"
                        className="btn btn-ghost"
                        onClick={() => {
                          const l = [...(entry.install ?? [])];
                          [l[i - 1], l[i]] = [l[i], l[i - 1]];
                          set({ install: l });
                        }}
                      >
                        Move up
                      </button>
                    )}
                    <button type="button" className="btn btn-ghost btn-danger" onClick={() => set({ install: (entry.install ?? []).filter((_, j) => j !== i) })}>
                      Remove option
                    </button>
                  </div>
                </div>
              ))}
              <button
                type="button"
                className="btn btn-quiet"
                onClick={() => set({ install: [...(entry.install ?? []), { type: "git", launch: "", setup: "", requires: [] }] })}
              >
                Add install option
              </button>
              <Notes findings={findings} field="install" />
            </fieldset>

            <fieldset className="form-section">
              <legend>Port</legend>
              <div className="form-grid">
                <label className="field">
                  <span>Fixed port (empty: the app reads PORT)</span>
                  <input
                    type="number"
                    min={0}
                    max={65535}
                    value={entry.port || ""}
                    onChange={(e) => set({ port: Number(e.target.value) || 0 })}
                  />
                </label>
                <label className="field">
                  <span>Page to open (default /)</span>
                  <input value={entry.path ?? ""} onChange={(e) => set({ path: e.target.value })} placeholder="/" />
                </label>
              </div>
              <Notes findings={findings} field="port" />
            </fieldset>

            <fieldset className="form-section">
              <legend>Cloud sync</legend>
              <p className="muted small">
                Easiest after the first run: use <strong>Files</strong> on the app's card to see what it writes and pick from there.
              </p>
              <div className="form-grid">
                <label className="field">
                  <span>Paths are relative to</span>
                  <select value={entry.cloud?.root === "app" ? "app" : "data"} onChange={(e) => setCloud({ root: e.target.value === "app" ? "app" : "" })}>
                    <option value="data">the app's data folder (DATA_DIR)</option>
                    <option value="app">the app's own folder</option>
                  </select>
                </label>
                <label className="field">
                  <span>Size limit in MB (empty: none)</span>
                  <input
                    type="number"
                    min={0}
                    value={entry.cloud?.maxSizeMB || ""}
                    onChange={(e) => setCloud({ maxSizeMB: Number(e.target.value) || 0 })}
                  />
                </label>
                <label className="field">
                  <span>Sync these (one pattern per line, ** = any depth)</span>
                  <textarea rows={3} value={(entry.cloud?.include ?? []).join("\n")} onChange={(e) => setCloud({ include: lines(e.target.value) })} />
                </label>
                <label className="field">
                  <span>Except these</span>
                  <textarea rows={3} value={(entry.cloud?.exclude ?? []).join("\n")} onChange={(e) => setCloud({ exclude: lines(e.target.value) })} />
                </label>
                <label className="field">
                  <span>SQLite databases among them (exact paths)</span>
                  <textarea rows={2} value={(entry.cloud?.sqlite ?? []).join("\n")} onChange={(e) => setCloud({ sqlite: lines(e.target.value) })} />
                </label>
              </div>
              <Notes findings={findings} field="cloud" />
            </fieldset>
          </>
        )}

        <div className="modal-actions">
          <button type="button" className="btn btn-quiet" onClick={onClose}>
            Cancel
          </button>
          {entry && (
            <button type="submit" className="btn btn-primary" disabled={busy}>
              {mode === "add" ? "Add to library" : "Save"}
            </button>
          )}
        </div>
      </form>
    </Modal>
  );
}
