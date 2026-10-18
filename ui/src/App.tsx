import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, ApiError, AppView, ConfirmInfo, LibraryEntry, Settings, Status } from "./api";
import { AppCard, Busy } from "./components/AppCard";
import { LoadingScreen } from "./components/LoadingScreen";
import { Modal } from "./components/Modal";
import { ProfilePanel } from "./components/ProfilePanel";
import { AppForm } from "./components/AppForm";
import { FilesDialog } from "./components/FilesDialog";

type Dialog =
  | { kind: "confirm"; app: AppView; info: ConfirmInfo }
  | { kind: "token"; app: AppView; message: string; action: "install" | "update" }
  | { kind: "error"; title: string; message: string; log?: string[] }
  | { kind: "logs"; app: AppView; lines: string[] }
  | { kind: "add" }
  | { kind: "edit"; app: AppView; entry: LibraryEntry }
  | { kind: "files"; app: AppView }
  | { kind: "remove"; app: AppView }
  | { kind: "delete"; app: AppView }
  | { kind: "deleted"; app: AppView; report: string[] };

/** Opens a tab synchronously inside the click (popup blockers allow that). */
function openPendingTab(name: string): Window | null {
  const tab = window.open("", "_blank");
  if (!tab) return null;
  tab.opener = null;
  tab.document.title = `Starting ${name}…`;
  tab.document.body.style.cssText =
    "margin:0;min-height:100vh;display:grid;place-items:center;background:#05070d;color:#8a96b8;font:16px system-ui,sans-serif";
  tab.document.body.textContent = `Starting ${name}…`;
  return tab;
}

export function App() {
  const [status, setStatus] = useState<Status | null>(null);
  const [apps, setApps] = useState<AppView[]>([]);
  const [groups, setGroups] = useState<string[]>([]);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [connError, setConnError] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [group, setGroup] = useState<string | null>(null);
  const [busy, setBusy] = useState<Record<string, Busy>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [profileOpen, setProfileOpen] = useState(false);
  const [dialog, setDialog] = useState<Dialog | null>(null);
  const [shutDown, setShutDown] = useState(false);
  const [dismissed, setDismissed] = useState<string[]>([]);
  const lastVersion = useRef(-1);

  const refreshApps = useCallback(async () => {
    const r = await api.apps();
    setApps(r.apps ?? []);
    setGroups(r.groups ?? []);
    lastVersion.current = r.version;
  }, []);

  const refreshSettings = useCallback(async () => {
    try {
      setSettings(await api.settings());
    } catch {
      /* shown elsewhere */
    }
  }, []);

  // Poll status: fast while starting up, slower afterwards. The version
  // number tells us when the app list needs reloading.
  useEffect(() => {
    if (shutDown) return;
    let alive = true;
    let timer: number | undefined;
    const tick = async () => {
      try {
        const s = await api.status();
        if (!alive) return;
        setStatus(s);
        setConnError(null);
        if (s.ready && s.version !== lastVersion.current) await refreshApps();
        timer = window.setTimeout(tick, s.ready ? 1500 : 400);
      } catch (e) {
        if (!alive) return;
        setConnError((e as Error).message);
        timer = window.setTimeout(tick, 2000);
      }
    };
    tick();
    return () => {
      alive = false;
      window.clearTimeout(timer);
    };
  }, [refreshApps, shutDown]);

  useEffect(() => {
    if (status?.ready) refreshSettings();
  }, [status?.ready, refreshSettings]);

  // Settings change when Drive sign-in finishes in another tab.
  useEffect(() => {
    if (!profileOpen) return;
    const onFocus = () => refreshSettings();
    window.addEventListener("focus", onFocus);
    const t = window.setInterval(refreshSettings, 3000);
    return () => {
      window.removeEventListener("focus", onFocus);
      window.clearInterval(t);
    };
  }, [profileOpen, refreshSettings]);

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    return apps.filter((a) => (!group || a.group === group) && (!q || a.name.toLowerCase().includes(q)));
  }, [apps, query, group]);

  const setAppBusy = (id: string, b: Busy) => setBusy((m) => ({ ...m, [id]: b }));
  const setAppError = (id: string, e?: string) =>
    setErrors((m) => {
      const n = { ...m };
      if (e) n[id] = e;
      else delete n[id];
      return n;
    });

  const replaceApp = (a: AppView) => setApps((list) => list.map((x) => (x.id === a.id ? a : x)));

  // ----- install -----
  const install = async (app: AppView, confirm: boolean, token?: string) => {
    setAppError(app.id);
    if (confirm) setAppBusy(app.id, "installing");
    try {
      const r = await api.install(app.id, confirm, token);
      replaceApp(r.app); // card flips to Stopped + Start, no reload
    } catch (e) {
      const err = e as ApiError;
      if (err.confirm) setDialog({ kind: "confirm", app, info: err.confirm });
      else if (err.needsToken) setDialog({ kind: "token", app, message: err.message, action: "install" });
      else if (err.log && err.log.length)
        setDialog({ kind: "error", title: `Could not install ${app.name}`, message: err.message, log: err.log });
      else setAppError(app.id, err.message);
    } finally {
      setAppBusy(app.id, undefined);
      refreshApps().catch(() => {});
    }
  };

  // ----- update -----
  const update = async (app: AppView, confirm: boolean, token?: string) => {
    setAppError(app.id);
    if (confirm) setAppBusy(app.id, "updating");
    try {
      const r = await api.update(app.id, confirm, token);
      replaceApp(r.app);
    } catch (e) {
      const err = e as ApiError;
      if (err.confirm) setDialog({ kind: "confirm", app, info: err.confirm });
      else if (err.needsToken) setDialog({ kind: "token", app, message: err.message, action: "update" });
      else if (err.log && err.log.length)
        setDialog({ kind: "error", title: `Could not update ${app.name}`, message: err.message, log: err.log });
      else setAppError(app.id, err.message);
    } finally {
      setAppBusy(app.id, undefined);
      refreshApps().catch(() => {});
    }
  };

  // ----- launch -----
  const start = async (app: AppView, confirm: boolean) => {
    const tab = openPendingTab(app.name);
    setAppError(app.id);
    setAppBusy(app.id, "starting");
    try {
      const { url } = await api.launch(app.id, confirm);
      if (tab && !tab.closed) tab.location.href = url;
      else window.open(url, "_blank", "noopener");
    } catch (e) {
      tab?.close();
      const err = e as ApiError;
      if (err.confirm) setDialog({ kind: "confirm", app, info: err.confirm });
      else setDialog({ kind: "error", title: `Could not start ${app.name}`, message: err.message });
    } finally {
      setAppBusy(app.id, undefined);
      refreshApps().catch(() => {});
    }
  };

  const stop = async (app: AppView, force: boolean) => {
    setAppBusy(app.id, "stopping");
    try {
      const r = force ? await api.forceStop(app.id) : await api.stop(app.id);
      replaceApp(r.app);
    } catch (e) {
      setAppError(app.id, (e as Error).message);
    } finally {
      setAppBusy(app.id, undefined);
      refreshApps().catch(() => {});
    }
  };

  const syncNow = async (app: AppView) => {
    setAppError(app.id);
    try {
      const r = await api.sync(app.id);
      replaceApp(r.app);
    } catch (e) {
      setAppError(app.id, (e as Error).message);
    }
  };

  const editApp = async (app: AppView) => {
    try {
      setDialog({ kind: "edit", app, entry: await api.getConfig(app.id) });
    } catch (e) {
      setAppError(app.id, (e as Error).message);
    }
  };

  const removeApp = async (app: AppView) => {
    setDialog(null);
    try {
      await api.removeApp(app.id);
      await refreshApps();
    } catch (e) {
      setAppError(app.id, (e as Error).message);
    }
  };

  const showLogs = async (app: AppView) => {
    try {
      const r = await api.logs(app.id);
      setDialog({ kind: "logs", app, lines: r.lines });
    } catch (e) {
      setAppError(app.id, (e as Error).message);
    }
  };

  const shutdown = async () => {
    try {
      await api.shutdown();
    } catch {
      /* the host may already be gone */
    }
    setProfileOpen(false);
    setShutDown(true);
  };

  if (shutDown) {
    return (
      <main className="loading">
        <div className="loading-box">
          <div className="brand brand-lg">
            <span className="brand-mark" aria-hidden="true" />
            MyDailyApps
          </div>
          <h1 className="shutdown-title">MyDailyApps has shut down</h1>
          <p className="muted">All apps were stopped. You can close this tab.</p>
        </div>
      </main>
    );
  }

  if (!status?.ready) return <LoadingScreen status={status} error={connError} />;

  const notices = status.notices.filter((n) => !dismissed.includes(n));
  const pic = settings?.profilePicture;

  return (
    <div className="page">
      <header className="topbar">
        <div className="brand">
          <span className="brand-mark" aria-hidden="true" />
          MyDailyApps
        </div>
        {status.mode === "guest" && <span className="badge" title="Everything is removed when you shut down">Guest</span>}
        <div className="spacer" />
        <button
          type="button"
          className="avatar avatar-btn"
          aria-label="Profile and settings"
          aria-haspopup="dialog"
          aria-expanded={profileOpen}
          onClick={() => setProfileOpen(true)}
        >
          {pic ? (
            <img src={pic} alt="" />
          ) : (
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true">
              <circle cx="12" cy="8" r="4" />
              <path d="M4 21c1-4 4-6 8-6s7 2 8 6" />
            </svg>
          )}
        </button>
      </header>

      <main className="content">
        <h1 className="sr-only">Your apps</h1>
        {connError && (
          <div className="notice notice-error" role="alert">
            Lost the connection to the host: {connError}
          </div>
        )}
        {notices.map((n) => (
          <div key={n} className="notice" role="status">
            <span>{n}</span>
            <button type="button" className="link-btn" onClick={() => setDismissed((d) => [...d, n])}>
              Dismiss
            </button>
          </div>
        ))}

        <div className="filters" role="search">
          <div className="search">
            <label htmlFor="q" className="sr-only">
              Search apps by name
            </label>
            <input id="q" type="search" placeholder="Search by name" value={query} onChange={(e) => setQuery(e.target.value)} />
          </div>
          <button type="button" className="btn btn-outline add-app" onClick={() => setDialog({ kind: "add" })}>
            Add app
          </button>
          <div className="chips" role="group" aria-label="Filter by group">
            <button type="button" className={`chip${group === null ? " chip-on" : ""}`} aria-pressed={group === null} onClick={() => setGroup(null)}>
              All
            </button>
            {groups.map((g) => (
              <button key={g} type="button" className={`chip${group === g ? " chip-on" : ""}`} aria-pressed={group === g} onClick={() => setGroup(g)}>
                {g}
              </button>
            ))}
          </div>
        </div>

        {apps.length === 0 ? (
          <div className="empty">
            <h2>No apps in your library yet</h2>
            <p className="muted">
              Put a <code>library.json</code> next to MyDailyApps, import one from the profile panel, or sign in to Google Drive to restore yours.
            </p>
          </div>
        ) : visible.length === 0 ? (
          <div className="empty">
            <p className="muted">No apps match the search and group filter.</p>
          </div>
        ) : (
          <div className="grid">
            {visible.map((a) => (
              <AppCard
                key={a.id}
                app={a}
                busy={busy[a.id]}
                error={errors[a.id]}
                onInstall={() => install(a, false)}
                onStart={() => start(a, false)}
                onStop={() => stop(a, false)}
                onForceStop={() => stop(a, true)}
                onLogs={() => showLogs(a)}
                onSync={() => syncNow(a)}
                onUpdate={() => update(a, false)}
                onEdit={() => editApp(a)}
                onFiles={() => setDialog({ kind: "files", app: a })}
                onRemove={() => setDialog({ kind: "remove", app: a })}
                onDelete={() => setDialog({ kind: "delete", app: a })}
              />
            ))}
          </div>
        )}
      </main>

      {profileOpen && (
        <ProfilePanel
          settings={settings}
          onSettings={setSettings}
          onClose={() => setProfileOpen(false)}
          onShutdown={shutdown}
          onLibraryChanged={() => refreshApps().catch(() => {})}
          checkingUpdates={status.checkingUpdates}
          libraryNote={status.library}
        />
      )}

      {dialog?.kind === "confirm" && (
        <ConfirmDialog
          info={dialog.info}
          app={dialog.app}
          onCancel={() => setDialog(null)}
          onConfirm={() => {
            const d = dialog;
            setDialog(null);
            if (d.info.kind === "install") install(d.app, true);
            else if (d.info.kind === "update") update(d.app, true);
            else start(d.app, true);
          }}
        />
      )}
      {dialog?.kind === "token" && (
        <TokenDialog
          app={dialog.app}
          message={dialog.message}
          onCancel={() => setDialog(null)}
          onSubmit={(t) => {
            const d = dialog;
            setDialog(null);
            if (d.action === "update") update(d.app, true, t);
            else install(d.app, true, t);
          }}
        />
      )}
      {dialog?.kind === "error" && (
        <Modal title={dialog.title} onClose={() => setDialog(null)} wide={!!dialog.log}>
          <p className="pre-wrap">{dialog.message}</p>
          {dialog.log && <pre className="log">{dialog.log.join("\n")}</pre>}
          <div className="modal-actions">
            <button type="button" className="btn btn-primary" onClick={() => setDialog(null)}>
              OK
            </button>
          </div>
        </Modal>
      )}
      {(dialog?.kind === "add" || dialog?.kind === "edit") && (
        <AppForm
          mode={dialog.kind}
          initial={dialog.kind === "edit" ? dialog.entry : undefined}
          groups={groups}
          onClose={() => setDialog(null)}
          onSaved={() => {
            setDialog(null);
            refreshApps().catch(() => {});
          }}
        />
      )}
      {dialog?.kind === "files" && (
        <FilesDialog app={dialog.app} onClose={() => setDialog(null)} onChanged={() => refreshApps().catch(() => {})} />
      )}
      {dialog?.kind === "remove" && (
        <Modal title={`Remove ${dialog.app.name}?`} onClose={() => setDialog(null)}>
          <p>
            The card is taken out of your library. Nothing else is deleted: the app's folder (<code>{dialog.app.folder}</code>), its data and its
            files in Google Drive all stay.
          </p>
          <p className="muted small">
            If you add the same app again later, MyDailyApps finds its earlier settings and Drive files, and your data comes back. To delete everything
            instead, use <strong>Delete everything</strong>.
          </p>
          <div className="modal-actions">
            <button type="button" className="btn btn-quiet" onClick={() => setDialog(null)}>
              Cancel
            </button>
            <button type="button" className="btn btn-primary" onClick={() => removeApp(dialog.app)}>
              Remove
            </button>
          </div>
        </Modal>
      )}
      {dialog?.kind === "delete" && (
        <DeleteDialog
          app={dialog.app}
          onCancel={() => setDialog(null)}
          onDone={(report) => {
            setDialog({ kind: "deleted", app: dialog.app, report });
            refreshApps().catch(() => {});
          }}
        />
      )}
      {dialog?.kind === "deleted" && (
        <Modal title={`${dialog.app.name} was deleted`} onClose={() => setDialog(null)}>
          <ul className="report">
            {dialog.report.map((r, i) => (
              <li key={i}>{r}</li>
            ))}
          </ul>
          <div className="modal-actions">
            <button type="button" className="btn btn-primary" onClick={() => setDialog(null)}>
              OK
            </button>
          </div>
        </Modal>
      )}
      {dialog?.kind === "logs" && (
        <Modal title={`${dialog.app.name}: output`} onClose={() => setDialog(null)} wide>
          <pre className="log">{dialog.lines.length ? dialog.lines.join("\n") : "No output yet."}</pre>
          <div className="modal-actions">
            <button type="button" className="btn btn-quiet" onClick={() => showLogs(dialog.app)}>
              Refresh
            </button>
            <button type="button" className="btn btn-primary" onClick={() => setDialog(null)}>
              Close
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}

function ConfirmDialog({ info, app, onCancel, onConfirm }: { info: ConfirmInfo; app: AppView; onCancel: () => void; onConfirm: () => void }) {
  const isInstall = info.kind === "install";
  const isUpdate = info.kind === "update";
  const title = isInstall ? `Install ${app.name}?` : isUpdate ? `Update ${app.name}?` : `Run ${app.name} for the first time?`;
  return (
    <Modal title={title} onClose={onCancel} wide>
      <p>
        Apps run code on this computer. Check these commands before continuing{app.repo ? <> (from <code>{app.repo}</code>)</> : null}.
      </p>
      <p className="muted small">
        Folder: <code>{info.folder}</code>
      </p>
      {(isInstall || isUpdate) && (
        <>
          <h3 className="small-h">{isUpdate ? "Update steps" : "Install steps"}</h3>
          <pre className="log">{info.commands.join("\n")}</pre>
        </>
      )}
      <h3 className="small-h">Start command</h3>
      <pre className="log">{info.launch}</pre>
      {info.cloud && info.cloud.length > 0 && (
        <>
          <h3 className="small-h">Files kept in your Google Drive (inside the app's data folder)</h3>
          <pre className="log">{info.cloud.join("\n")}</pre>
        </>
      )}
      <div className="modal-actions">
        <button type="button" className="btn btn-quiet" onClick={onCancel}>
          Cancel
        </button>
        <button type="button" className="btn btn-primary" onClick={onConfirm}>
          {isInstall ? "Install" : isUpdate ? "Update" : "Run"}
        </button>
      </div>
    </Modal>
  );
}

function DeleteDialog({ app, onCancel, onDone }: { app: AppView; onCancel: () => void; onDone: (report: string[]) => void }) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (typed.trim() !== app.id) return;
    setBusy(true);
    setError(null);
    try {
      const r = await api.deleteEverything(app.id);
      onDone(r.report);
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };
  return (
    <Modal title={`Delete ${app.name} everywhere?`} onClose={onCancel}>
      <form onSubmit={submit} className="form">
        <p>This deletes, and cannot be undone on this computer:</p>
        <ul className="report">
          <li>
            the app's folder <code>{app.folder}</code>
          </li>
          <li>its data folder and everything MyDailyApps remembers about it here</li>
          <li>its card in your library</li>
          <li>
            its files in Google Drive (<code>MyDailyApps/apps/{app.id}</code> and its backups). They go to the Drive trash, where Google keeps them for 30
            days.
          </li>
        </ul>
        <label className="field">
          <span>
            Type <code>{app.id}</code> to confirm
          </span>
          <input value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false} />
        </label>
        {error && (
          <p className="error-text" role="alert">
            {error}
          </p>
        )}
        <div className="modal-actions">
          <button type="button" className="btn btn-quiet" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary btn-delete" disabled={busy || typed.trim() !== app.id}>
            {busy ? "Deleting…" : "Delete everything"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function TokenDialog({ app, message, onCancel, onSubmit }: { app: AppView; message: string; onCancel: () => void; onSubmit: (t: string) => void }) {
  const [token, setToken] = useState("");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (token.trim()) onSubmit(token.trim());
  };
  return (
    <Modal title={`Token needed for ${app.name}`} onClose={onCancel}>
      <form onSubmit={submit} className="form">
        <p>{message}</p>
        <label className="field">
          <span>GitHub token</span>
          <input type="password" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" spellCheck={false} />
        </label>
        <p className="muted small">Kept in memory for this session only and never saved.</p>
        <div className="modal-actions">
          <button type="button" className="btn btn-quiet" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={!token.trim()}>
            Install
          </button>
        </div>
      </form>
    </Modal>
  );
}
