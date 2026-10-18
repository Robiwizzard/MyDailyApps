import { useEffect, useRef, useState } from "react";
import { api, Settings } from "../api";
import { Modal } from "./Modal";

interface Props {
  settings: Settings | null;
  onSettings: (s: Settings) => void;
  onClose: () => void;
  onShutdown: () => void;
  onLibraryChanged: () => void;
  libraryNote: string;
}

/** Resize a picked image to a small square data URL (keeps the stored picture tiny). */
async function toAvatar(file: File): Promise<string> {
  const url = URL.createObjectURL(file);
  try {
    const img = new Image();
    await new Promise<void>((ok, fail) => {
      img.onload = () => ok();
      img.onerror = () => fail(new Error("That file is not an image the browser can read."));
      img.src = url;
    });
    const size = 256;
    const c = document.createElement("canvas");
    c.width = c.height = size;
    const ctx = c.getContext("2d")!;
    const s = Math.min(img.width, img.height);
    ctx.drawImage(img, (img.width - s) / 2, (img.height - s) / 2, s, s, 0, 0, size, size);
    return c.toDataURL("image/jpeg", 0.85);
  } finally {
    URL.revokeObjectURL(url);
  }
}

export function ProfilePanel({ settings, onSettings, onClose, onShutdown, onLibraryChanged, libraryNote }: Props) {
  const panelRef = useRef<HTMLElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const libRef = useRef<HTMLInputElement>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [confirmShutdown, setConfirmShutdown] = useState(false);

  // Focus the panel once when it opens (not on every re-render of the page).
  useEffect(() => {
    panelRef.current?.querySelector<HTMLElement>("button")?.focus();
  }, []);

  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !confirmShutdown) closeRef.current();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [confirmShutdown]);

  const run = async (fn: () => Promise<void>) => {
    setError(null);
    setInfo(null);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const pickPicture = () => fileRef.current?.click();
  const onPicture = (f: File | undefined) =>
    f && run(async () => onSettings(await api.saveSettings({ profilePicture: await toAvatar(f) })));

  const connect = () =>
    run(async () => {
      // Open the tab during the click so popup blockers allow it.
      const tab = window.open("", "_blank");
      try {
        const { authUrl } = await api.driveConnect();
        if (tab) {
          tab.opener = null;
          tab.location.href = authUrl;
        } else {
          window.location.href = authUrl;
        }
        setInfo("Finish signing in in the new tab. This panel updates when you are done.");
      } catch (e) {
        tab?.close();
        throw e;
      }
    });

  const importLibrary = (f: File | undefined) =>
    f &&
    run(async () => {
      await api.importLibrary(await f.text());
      onLibraryChanged();
      setInfo(`Imported ${f.name}.`);
    });

  const pic = settings?.profilePicture;

  return (
    <div className="overlay overlay-side" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <aside ref={panelRef} className="panel" role="dialog" aria-modal="true" aria-label="Profile and settings">
        <div className="panel-head">
          <div className="avatar avatar-lg" aria-hidden="true">
            {pic ? <img src={pic} alt="" /> : <span>{(settings?.drive.email?.[0] ?? "M").toUpperCase()}</span>}
          </div>
          <div className="panel-title">
            <h2>Profile</h2>
            <button type="button" className="link-btn" onClick={pickPicture}>
              {pic ? "Change picture" : "Add picture"}
            </button>
            {pic && (
              <button type="button" className="link-btn link-muted" onClick={() => run(async () => onSettings(await api.saveSettings({ profilePicture: "" })))}>
                Remove
              </button>
            )}
            <input ref={fileRef} type="file" accept="image/*" hidden onChange={(e) => onPicture(e.target.files?.[0])} />
          </div>
          <button type="button" className="icon-btn" aria-label="Close" onClick={onClose}>
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
              <path d="M6 6l12 12M18 6L6 18" />
            </svg>
          </button>
        </div>

        {error && <p className="error-text" role="alert">{error}</p>}
        {info && <p className="info-text" role="status">{info}</p>}

        <section className="panel-section" aria-labelledby="drive-h">
          <h3 id="drive-h">Google Drive</h3>
          {!settings?.drive.configured ? (
            <p className="muted small">
              Sign-in is not set up for this copy. Set MYDAILYAPPS_GOOGLE_CLIENT_ID and MYDAILYAPPS_GOOGLE_CLIENT_SECRET (see README).
            </p>
          ) : settings.drive.connected ? (
            <>
              <div className="row-between">
                <div className="status status-running">
                  <span className="status-dot" aria-hidden="true" />
                  <span>Connected{settings.drive.email ? ` as ${settings.drive.email}` : ""}</span>
                </div>
                <button type="button" className="btn btn-quiet" onClick={() => run(async () => onSettings(await api.driveDisconnect()))}>
                  Sign out
                </button>
              </div>
              <label className="check">
                <input
                  type="checkbox"
                  checked={settings.syncPicture}
                  onChange={(e) => run(async () => onSettings(await api.saveSettings({ syncPicture: e.target.checked })))}
                />
                Keep the profile picture in Drive too
              </label>
            </>
          ) : (
            <div className="row-between">
              <div className="status status-stopped">
                <span className="status-dot" aria-hidden="true" />
                <span>Not connected</span>
              </div>
              <button type="button" className="btn btn-primary" onClick={connect}>
                Sign in
              </button>
            </div>
          )}
        </section>

        <section className="panel-section" aria-labelledby="lib-h">
          <h3 id="lib-h">Library</h3>
          <p className="muted small">{!libraryNote || libraryNote === "empty" ? "No library loaded yet." : `Loaded from ${libraryNote}.`}</p>
          <div className="btn-row">
            <button type="button" className="btn btn-quiet" onClick={() => libRef.current?.click()}>
              Import file
            </button>
            <a className="btn btn-quiet" href="/api/library" download="library.json">
              Export
            </a>
            <button type="button" className="btn btn-quiet" onClick={() => run(async () => { await api.rescan(); onLibraryChanged(); setInfo("Apps folder scanned."); })}>
              Rescan folder
            </button>
            {settings?.drive.connected && (
              <button type="button" className="btn btn-quiet" onClick={() => run(async () => { await api.driveSync(); onLibraryChanged(); setInfo("Library reloaded from Drive."); })}>
                Reload from Drive
              </button>
            )}
          </div>
          <input ref={libRef} type="file" accept="application/json,.json" hidden onChange={(e) => importLibrary(e.target.files?.[0])} />
          {settings && (
            <p className="muted small path">
              Apps folder: <code>{settings.appsDir}</code>
              <br />
              Mode: {settings.mode === "guest" ? "guest (everything is removed when you shut down)" : "persistent"}
            </p>
          )}
        </section>

        <button type="button" className="btn btn-primary btn-block" onClick={() => setConfirmShutdown(true)}>
          Shut down MyDailyApps
        </button>
      </aside>

      {confirmShutdown && (
        <Modal title="Shut down MyDailyApps?" onClose={() => setConfirmShutdown(false)}>
          <p>All running apps will be stopped and the host will exit.</p>
          <div className="modal-actions">
            <button type="button" className="btn btn-quiet" onClick={() => setConfirmShutdown(false)}>
              Cancel
            </button>
            <button type="button" className="btn btn-primary" onClick={onShutdown}>
              Shut down
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
