import { useState } from "react";
import type { AppView } from "../api";

export type Busy = "installing" | "starting" | "stopping" | "updating" | undefined;

interface Props {
  app: AppView;
  busy: Busy;
  error?: string;
  onInstall: () => void;
  onStart: () => void;
  onStop: () => void;
  onForceStop: () => void;
  onLogs: () => void;
  onSync: () => void;
  onUpdate: () => void;
  onEdit: () => void;
  onFiles: () => void;
  onRemove: () => void;
  onDelete: () => void;
}

const statusText: Record<string, string> = {
  not_installed: "Not installed",
  installing: "Installing…",
  stopped: "Stopped",
  starting: "Starting…",
  running: "Running",
  stopping: "Stopping…",
  syncing: "Syncing…",
  updating: "Updating…",
};

export function AppCard({ app, busy, error, onInstall, onStart, onStop, onForceStop, onLogs, onSync, onUpdate, onEdit, onFiles, onRemove, onDelete }: Props) {
  const [more, setMore] = useState(false);
  const status =
    busy ??
    (app.updating ? "updating" : app.syncing && (app.status === "stopped" || app.status === "starting") ? "syncing" : app.status);
  const running = app.status === "running" || app.status === "starting";
  const initial = (app.name.trim()[0] ?? "?").toUpperCase();

  // One action button, always in the same spot.
  let action = (
    <button type="button" className="btn btn-outline" onClick={onInstall} disabled={!!busy}>
      Install
    </button>
  );
  if (status === "installing" || status === "updating") {
    action = (
      <button type="button" className="btn btn-outline" disabled>
        {status === "updating" ? "Updating…" : "Installing…"}
      </button>
    );
  } else if (status === "syncing") {
    action = (
      <button type="button" className="btn btn-primary" disabled>
        Syncing…
      </button>
    );
  } else if (app.status === "stopped" && !busy) {
    action = (
      <button type="button" className="btn btn-primary" onClick={onStart}>
        Start
      </button>
    );
  } else if (status === "starting") {
    action = (
      <button type="button" className="btn btn-primary" disabled>
        Starting…
      </button>
    );
  } else if (running || status === "stopping") {
    action = (
      <button type="button" className="btn btn-outline" onClick={onStop} disabled={status === "stopping"}>
        {status === "stopping" ? "Stopping…" : "Stop"}
      </button>
    );
  }

  return (
    <article className={`card${app.status === "running" ? " card-running" : ""}`} aria-label={app.name}>
      <div className="card-top">
        <div className="card-icon" aria-hidden="true">
          {initial}
        </div>
        <div className="card-title">
          {app.status === "running" && app.url ? (
            <a className="card-name" href={app.url} target="_blank" rel="noopener noreferrer" title="Open in a new tab">
              {app.name}
            </a>
          ) : (
            <h2 className="card-name">{app.name}</h2>
          )}
          <div className="card-group">{app.group || "No group"}</div>
        </div>
        {(
          <button
            type="button"
            className="icon-btn card-more"
            aria-label={`More actions for ${app.name}`}
            aria-expanded={more}
            onClick={() => setMore((m) => !m)}
          >
            <svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
              <circle cx="5" cy="12" r="1.8" />
              <circle cx="12" cy="12" r="1.8" />
              <circle cx="19" cy="12" r="1.8" />
            </svg>
          </button>
        )}
      </div>
      {app.description && <p className="card-desc">{app.description}</p>}
      {more && (
        <div className="card-extra">
          {app.status === "running" && app.url && (
            <a className="btn btn-ghost" href={app.url} target="_blank" rel="noopener noreferrer">
              Open
            </a>
          )}
          {app.status !== "not_installed" && (
            <>
              <button type="button" className="btn btn-ghost" onClick={onFiles}>
                Files
              </button>
              <button type="button" className="btn btn-ghost" onClick={onLogs}>
                Logs
              </button>
            </>
          )}
          <button type="button" className="btn btn-ghost" onClick={onEdit}>
            Edit
          </button>
          {app.cloud && app.status === "stopped" && (
            <button type="button" className="btn btn-ghost" onClick={onSync} disabled={!!app.syncing}>
              Sync now
            </button>
          )}
          {running && (
            <button type="button" className="btn btn-ghost btn-danger" onClick={onForceStop}>
              Force stop
            </button>
          )}
          {!running && (
            <>
              <button type="button" className="btn btn-ghost" onClick={onRemove}>
                Remove
              </button>
              <button type="button" className="btn btn-ghost btn-danger" onClick={onDelete}>
                Delete everything
              </button>
            </>
          )}
        </div>
      )}
      {(error || app.error) && (
        <p className="card-error" role="alert">
          {error || app.error}
        </p>
      )}
      {app.syncNote && !(error || app.error) && (
        <p className={app.syncNote.startsWith("Sync failed") ? "card-error" : "card-note"}>{app.syncNote}</p>
      )}
      {app.update && status !== "updating" && (
        <p className="card-update">Update available: {app.update}</p>
      )}
      <div className="card-bottom">
        <div className={`status status-${status}`}>
          <span className="status-dot" aria-hidden="true" />
          <span>{statusText[status] ?? status}</span>
        </div>
        <div className="card-actions">
          {app.update && status !== "updating" && status !== "not_installed" && (
            <button
              type="button"
              className="btn btn-outline"
              onClick={onUpdate}
              disabled={app.status !== "stopped" || !!busy}
              title={app.status !== "stopped" ? "Stop the app to update it" : `Update ${app.name}`}
            >
              Update
            </button>
          )}
          {action}
        </div>
      </div>
    </article>
  );
}
