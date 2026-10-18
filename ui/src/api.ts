// Typed client for the host API. Every state-changing request carries the
// X-MyDailyApps header, which the host requires (blocks cross-site requests).

export type AppStatus = "not_installed" | "installing" | "stopped" | "starting" | "running";

export interface AppView {
  id: string;
  name: string;
  group: string;
  description?: string;
  repo?: string;
  status: AppStatus;
  url?: string;
  folder: string;
  lastLaunched?: string;
  error?: string;
  cloud: boolean;
  syncing?: boolean;
  syncNote?: string;
  update?: string; // set when a newer version is available, e.g. "3 new commits"
  updating?: boolean;
}

export interface AppsResponse {
  apps: AppView[];
  groups: string[];
  version: number;
  ready: boolean;
}

export interface Step {
  id: string;
  label: string;
  state: "pending" | "running" | "done" | "error" | "skipped";
  detail?: string;
}

export interface Status {
  ready: boolean;
  steps: Step[];
  notices: string[];
  mode: "guest" | "persistent";
  version: number;
  library: string;
  checkingUpdates: boolean;
}

export interface Settings {
  profilePicture?: string;
  syncPicture: boolean;
  drive: { configured: boolean; connected: boolean; email?: string };
  mode: "guest" | "persistent";
  dataDir: string;
  appsDir: string;
}

export interface ConfirmInfo {
  kind: "install" | "launch" | "update";
  folder: string;
  commands: string[];
  launch: string;
  cloud?: string[];
}

/** Error from the API, with the extra fields some endpoints return. */
export class ApiError extends Error {
  status: number;
  confirm?: ConfirmInfo;
  needsToken?: boolean;
  missing?: string[];
  log?: string[];
  constructor(status: number, body: Record<string, unknown>) {
    super(typeof body.error === "string" ? body.error : `Request failed (${status})`);
    this.status = status;
    this.confirm = body.confirm as ConfirmInfo | undefined;
    this.needsToken = body.needsToken === true;
    this.missing = body.missing as string[] | undefined;
    this.log = body.log as string[] | undefined;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, headers: { "X-MyDailyApps": "1" } };
  if (body !== undefined) {
    init.body = typeof body === "string" ? body : JSON.stringify(body);
    (init.headers as Record<string, string>)["Content-Type"] = "application/json";
  }
  const res = await fetch(path, init);
  const text = await res.text();
  let data: Record<string, unknown> = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    data = { error: text };
  }
  if (!res.ok) throw new ApiError(res.status, data);
  return data as T;
}

export interface InstallOption {
  type: "release" | "git";
  requires?: string[];
  setup?: string;
  launch: string;
  asset?: string;
  tag?: string;
}

export interface CloudSpec {
  root?: "" | "data" | "app";
  include: string[];
  exclude?: string[];
  sqlite?: string[];
  maxSizeMB?: number;
}

/** A library entry: the app's whole configuration. */
export interface LibraryEntry {
  id: string;
  name?: string;
  group?: string;
  description?: string;
  repo?: string;
  port?: number;
  path?: string;
  install?: InstallOption[];
  cloud?: CloudSpec | null;
}

export interface Finding {
  field: string;
  confidence: "detected" | "guessed" | "missing";
  note: string;
}

export interface Draft {
  app: LibraryEntry;
  findings: Finding[];
}

export interface FileView {
  key: string;
  where: "data" | "app";
  path: string;
  size: number;
  sqlite?: boolean;
  keep: boolean;
  reasons: string[];
  synced: boolean;
  aside?: boolean;
  question?: boolean;
  gone?: boolean;
}

export interface FilesInfo {
  runs: number;
  running: boolean;
  recording: boolean;
  root: "data" | "app";
  files: FileView[];
}

const enc = encodeURIComponent;

export const api = {
  status: () => request<Status>("GET", "/api/status"),
  apps: () => request<AppsResponse>("GET", "/api/apps"),
  rescan: () => request<AppsResponse>("POST", "/api/rescan"),
  install: (id: string, confirm: boolean, token?: string) =>
    request<{ app: AppView; log: string[] }>("POST", `/api/apps/${enc(id)}/install`, { confirm, token }),
  launch: (id: string, confirm: boolean) =>
    request<{ url: string }>("POST", `/api/apps/${enc(id)}/launch`, { confirm }),
  stop: (id: string) => request<{ app: AppView }>("POST", `/api/apps/${enc(id)}/stop`),
  forceStop: (id: string) => request<{ app: AppView }>("POST", `/api/apps/${enc(id)}/force-stop`),
  update: (id: string, confirm: boolean, token?: string) =>
    request<{ app: AppView; log: string[] }>("POST", `/api/apps/${enc(id)}/update`, { confirm, token }),
  checkUpdates: () => request<AppsResponse>("POST", "/api/updates/check"),
  detect: (repo: string, id?: string, token?: string) => request<Draft>("POST", "/api/detect", { repo, id, token }),
  addApp: (e: LibraryEntry) => request<AppsResponse>("POST", "/api/apps", e),
  getConfig: (id: string) => request<LibraryEntry>("GET", `/api/apps/${enc(id)}/config`),
  saveConfig: (id: string, e: LibraryEntry) => request<AppsResponse>("PUT", `/api/apps/${enc(id)}/config`, e),
  removeApp: (id: string) => request<AppsResponse>("DELETE", `/api/apps/${enc(id)}`),
  deleteEverything: (id: string) => request<{ report: string[] }>("POST", `/api/apps/${enc(id)}/delete-everything`, { confirm: id }),
  files: (id: string) => request<FilesInfo>("GET", `/api/apps/${enc(id)}/files`),
  record: (id: string, start: boolean) =>
    request<{ changed: string[] | null; files: FilesInfo }>("POST", `/api/apps/${enc(id)}/files/record`, { start }),
  aside: (id: string, key: string, action: "aside" | "restore" | "keep" | "temporary") =>
    request<FilesInfo>("POST", `/api/apps/${enc(id)}/files/aside`, { key, action }),
  applyFiles: (id: string, keys: string[]) => request<FilesInfo>("POST", `/api/apps/${enc(id)}/files/apply`, { keys }),
  sync: (id: string) => request<{ app: AppView }>("POST", `/api/apps/${enc(id)}/sync`),
  logs: (id: string) => request<{ lines: string[] }>("GET", `/api/apps/${enc(id)}/logs`),
  settings: () => request<Settings>("GET", "/api/settings"),
  saveSettings: (u: {
    profilePicture?: string;
    syncPicture?: boolean;
  }) => request<Settings>("PUT", "/api/settings", u),
  importLibrary: (json: string) => request<AppsResponse>("PUT", "/api/library", json),
  driveConnect: () => request<{ authUrl: string }>("POST", "/api/drive/connect"),
  driveDisconnect: () => request<Settings>("POST", "/api/drive/disconnect"),
  driveSync: () => request<AppsResponse>("POST", "/api/drive/sync"),
  shutdown: () => request<{ ok: boolean }>("POST", "/api/shutdown"),
};
