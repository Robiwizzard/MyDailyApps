# MyDailyApps

A launcher host for your own apps, like Steam but for your projects. It shows each app as a card, installs it when you click **Install**, starts it when you click **Start**, and opens it in a new browser tab. The apps are separate projects; the host only knows how to fetch and run them.

The host is **one executable** with the web UI built in. It runs on `localhost` for one user.

## Quick start (Windows)

1. Put `mydailyapps.exe` in a folder, for example `D:\Projects\MyDailyApps`.
2. Optional: copy `mydailyapps.env.example` to `mydailyapps.env` next to the exe and set `MYDAILYAPPS_MODE=persistent` so your data is kept between runs (the default, `guest`, wipes everything at exit).
3. Optional: copy `library.example.json` to `library.json` next to the exe and edit it to list your apps (or import one later from the profile panel).
4. Double-click `mydailyapps.exe`. A console window opens and your browser opens `http://localhost:8080`.
5. Stop it with **Shut down** in the profile panel, Ctrl+C in the console, or by closing the console window. All apps are stopped either way.

The first time you download the exe, Windows may show an "unknown publisher" warning because the file is not signed. Choose **More info → Run anyway** if you trust where it came from.

## Building from source

You need **Go 1.27.2+** and **Node.js 18+** (only to build; the finished exe needs neither).

```powershell
# Windows
powershell -ExecutionPolicy Bypass -File scripts\build.ps1
```

```sh
# macOS / Linux
./scripts/build.sh      # or: make build
```

This builds `ui/` with Vite into `ui/dist`, then builds the Go executable, which embeds `ui/dist`.

For UI work, run the host (`go run ./cmd/mydailyapps`) and in another terminal `cd ui && npm run dev`; Vite proxies `/api` to the host on port 8080.

Tests: `go test ./...` and `cd kit && go test ./...`.

## Configuration

Environment variables, optionally also read from `mydailyapps.env` next to the executable (`NAME=value` lines). The environment wins over the file.

| Variable | Meaning | Default |
|---|---|---|
| `MYDAILYAPPS_MODE` | `guest` = temp folder, wiped on exit. `persistent` = keep data | `guest` |
| `MYDAILYAPPS_DATA_DIR` | Local state, caches and tokens (persistent mode) | `%AppData%\MyDailyApps` (Windows), `~/Library/Application Support/MyDailyApps` (macOS), `~/.config/MyDailyApps` (Linux) |
| `MYDAILYAPPS_PORT` | Port for the UI and API | `8080` |
| `MYDAILYAPPS_APPS_DIR` | The apps folder: scanned at startup, used as install target (`<apps dir>/<app id>`) | `apps` next to the executable (a temp folder in guest mode) |
| `MYDAILYAPPS_OPEN_BROWSER` | Open the browser at startup | `true` |
| `MYDAILYAPPS_GOOGLE_CLIENT_ID` / `_SECRET` | Google OAuth client for Drive (see below) | none (Drive disabled) |

Guest mode leaves nothing behind: the temp folder (including installed apps and the Drive token) is removed at exit, and leftovers from a crashed guest session are removed at the next start.

## The library file

One JSON file lists which apps exist and how to install and launch them. It lives in Google Drive (`MyDailyApps/library.json`) and is cached locally. Without Drive the host reads the cache, then `library.json` next to the executable.

```json
{
  "version": 1,
  "apps": [
    {
      "id": "initiative-tracker",
      "name": "Initiative Tracker",
      "group": "D&D",
      "description": "Optional one-liner shown on the card",
      "repo": "https://github.com/OWNER/initiative-tracker",
      "install": [
        { "type": "release", "launch": "tracker" },
        { "type": "git", "requires": ["node"], "setup": "npm install", "launch": "npm start" }
      ]
    }
  ]
}
```

- `id`: lower case letters, digits, `.`, `_`, `-`. It is also the folder name under the apps folder.
- `group`: each app belongs to one group (the filter chips).
- `install`: tried in order; the first that works on this machine is used.
  - `release`: downloads the newest GitHub release asset whose name contains this OS and architecture (for example `tracker_windows_amd64.zip`) and unpacks it. Optional `asset` (extra text the name must contain) and `tag` (instead of latest).
  - `git`: `git clone`, then `setup`.
  - `requires`: tools that must be on the PATH. If none of the options works the card says what is missing ("Needs node, not found").
  - `launch`: run in the app folder. A program shipped in the folder (like `tracker` or `tracker.exe`) is found automatically.
- `port` (optional): a fixed port for apps that cannot read `PORT`. `path` (optional): the URL path to open.

Nothing is cloned or downloaded at startup. The host scans the apps folder and marks each app installed (the folder exists and has its launch program, or the host's install marker) or not installed. Apps you copy into the folder by hand count too; use **Rescan folder** in the profile panel after adding one.

Before an app runs for the first time, the host shows the exact install and start commands and asks you to confirm. Third-party apps run arbitrary code on your computer.

Private repositories: if a clone or release download needs authentication, the host asks for a GitHub token. It is kept in memory for the session only and never saved.

## Adding and editing apps

Each app's whole configuration is its **library entry**: name, group, install options, port and cloud files. The library syncs through Drive, so it works on any computer, before the app is installed. Nothing has to be added to the app's own repository.

**Add app** (next to the group filters):

1. Paste the repository link and click **Detect**.
2. The host reads the repository through the GitHub API. It doesn't clone it and runs nothing.
3. It drafts the entry with fixed rules, no AI. Each field is marked *Detected*, *Guessed* or *Missing*, with the reason.
4. Review, change anything, and save.

What the rules look at:

| From | Fills in |
|---|---|
| Repository name, description, topics | name, description, a group (an existing one if the topics match) |
| Newest release assets with OS and architecture in their names | a `release` install option and its start command |
| `package.json` (+ lockfile), `go.mod`, `Cargo.toml`, `pyproject.toml` / `requirements.txt` | a `git` option: required tools, setup and start commands, Node version note |
| The main source files | whether the app reads `PORT` (else the port it always uses) and `DATA_DIR` |
| `.gitignore` entries like `data/`, `saves/`, `*.db` | cloud patterns in the app's own folder, for apps that don't read `DATA_DIR` |

Without a GitHub token, GitHub allows 60 requests an hour; one detection uses up to about 17. A `mydailyapps.json` in the repository is still read as a hint when present.

**Edit** (card `⋯` menu) changes an entry later. **Detect again** reads the installed folder instead of GitHub.

**Remove** takes only the card out of the library. The app's folder, data and Drive files stay. Each app's Drive folder also keeps a copy of its settings (`MyDailyApps/apps/<id>/.mydailyapps-app.json`, refreshed whenever the entry changes and at startup). If you add the same app again (same id, or another id with the same repository), **Detect** finds that copy and brings back the earlier settings, so the old data syncs down on the first start. For folders from older versions without a settings copy, the sync patterns are rebuilt from the files in Drive.

**Delete everything** removes the app completely, after you type its id:

- **Drive:** its Drive folders (`apps/<id>` and `backups/<id>`) go to the Drive trash, where Google keeps them for 30 days. If Drive is connected and this step fails, nothing else is deleted.
- **This computer:** the app's folder, its data folder, what MyDailyApps remembers about it here, and its library entry are deleted.
- **Safety check:** folders that are not clearly the app's own (the apps folder itself, the data folder, anything containing MyDailyApps' own files) are refused.

## Files: choosing what to sync without reading the code

The card's `⋯` → **Files** shows what the app writes. The host snapshots the app's data folder and the files git doesn't track in its own folder, before each start and after each stop. Each file gets a *Keep* or *Temporary* suggestion with reasons:

- **Names and places:** `cache`, `logs`, `*.log`, `*.tmp`, database side files lean temporary. `saves`, `data`, `db`, SQLite files (found by their first bytes) lean keep.
- **Behaviour across runs:** a file that changes on every run whatever you do leans temporary. One that comes back with identical content after being removed was regenerated.
- **Record a save:** while the app runs, click **Record a save**, save something in the app, and click **Done saving**. Whatever changed in that window is marked keep.
- **Test without it:** with the app stopped, the file is moved aside. Run the app, then answer whether your data was still there. The answer decides, and the file is put back if it was needed.

**Use ticked files for cloud sync** writes the choice into the app's library entry as patterns. If every file seen in a folder is ticked, the whole folder is synced (`saves/**`).

## Cloud saves

Like Steam Cloud, the host keeps an app's data in Google Drive without the app talking to Google. The app's library entry lists which files to sync. They are under its `DATA_DIR`, or under its own folder with `"root": "app"` for apps that keep data next to their code:

```json
"cloud": {
  "include": ["cookbook.db", "images/**"],
  "exclude": ["images/cache/**"],
  "sqlite": ["cookbook.db"],
  "maxSizeMB": 500
}
```

How it works:

- **Before Start**, newer files are downloaded from `MyDailyApps/apps/<id>/` in Drive.
- **After the app exits**, files that changed are uploaded. SQLite databases listed under `sqlite` have their write-ahead log merged first, and are never copied while the app runs.
- **Sync now** (under the card's `⋯` button) does both while the app is stopped. The card shows the result of the last sync.
- **After the synced files change** (ticked in **Files**, or edited in **Add app**/**Edit**), a stopped app syncs right away, so new save paths reach Drive (and Drive's files reach this computer) without waiting for the next start.
- **Deletions**: a file that was synced before and is deleted on one side is deleted on the other side too. Cloud copies go to the Drive trash (Google keeps them for 30 days), never deleted for good. If the other side changed the file since the last sync, the change wins and the file comes back. As a safety net, when *every* synced file is gone from one side at once (an emptied data folder, or the Drive folder deleted on another computer), nothing is deleted on the other side; the files are copied back instead. Removing a pattern stops syncing those files but never deletes them.
- **Conflicts**: if a file changed both on this computer and in Drive since the last sync, the newer copy wins. The other copy is saved to `MyDailyApps/backups/<id>/` in Drive with the date and where it came from.
- **What isn't synced**: patterns can't leave their root folder. SQLite `-wal`/`-shm` files, `.git` and `node_modules` are never synced.
- **Size limit**: if the matched files are larger than `maxSizeMB`, nothing is uploaded and the card says why.
- **Errors**: if Drive can't be reached, the app starts with the local copy and the card shows the problem.

The first-run confirmation lists the synced patterns too, so you see what an app will keep in your Drive before it runs.

## Google Drive

Drive keeps the library (and optionally your profile picture) so a fresh computer can restore it after you sign in. The sign-in token stays on the computer and is never synced.

One-time setup in the [Google Cloud console](https://console.cloud.google.com/):

1. Create a project and enable the **Google Drive API**.
2. **OAuth consent screen**: user type External, add yourself as a test user, then **Publish app** (published apps keep sign-ins working; test-mode tokens expire after 7 days).
3. **Credentials → Create credentials → OAuth client ID → Desktop app**.
4. Put the client ID and secret in `mydailyapps.env` (`MYDAILYAPPS_GOOGLE_CLIENT_ID`, `MYDAILYAPPS_GOOGLE_CLIENT_SECRET`), or build them into a release with the `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` repository secrets.

Then open the profile panel and click **Sign in**.

The host asks only for the `drive.file` scope, so it can see **only the files it created itself**, never the rest of your Drive. That also means a `library.json` you upload to Drive by hand is invisible to it. Change the library from the profile panel instead: **Import file** saves it locally and to Drive, **Export** downloads the current one. On the first sign-in, if Drive has no library yet, the local one is uploaded.

## Updates

At startup the host checks every installed app for a newer version in the background. It only looks and never changes files:

- **Apps installed with git:** `git fetch`, then a count of the new commits on the branch.
- **Apps installed from a release:** a comparison of the installed release tag with the newest one on GitHub.

When a newer version exists, the card shows "Update available" and an **Update** button next to **Start**. Start keeps working, so you can keep using the current version.

Update works only while the app is stopped. It shows the exact steps and asks first:

- **git:** `git pull --ff-only`, then the app's setup command from its library entry. If you edited files in the app folder, the update stops instead of overwriting them.
- **release:** the new package is unpacked next to the app and set up, then swapped in. If anything fails, the old version stays.

**Check for updates** in the profile panel runs the check again. App data in `DATA_DIR` is never touched by an update.

## Running apps

- **Start** opens a blank tab right away (so popup blockers allow it), starts the app with `PORT` and `DATA_DIR` set, waits until the port answers (up to about 60 seconds), then points the tab at the app.
- **Stop** asks the app to exit and kills its whole process tree after about 5 seconds (npm and shell wrappers start child processes). **Force stop** (under the `⋯` button) kills it immediately. **Logs** shows the app's output.
- On Windows each app runs in a job object, so if the host is killed, Windows closes its apps too. On macOS/Linux each app gets its own process group.
- The host keeps a state file (`mydailyapps-host.json` in the temp folder). It stops a second copy of the host from starting (the second copy just opens the browser), and after a crash the next start reports "The last session ended unexpectedly" and stops leftover apps.

## Making your own apps

The app contract is small: read `PORT` and `DATA_DIR` from the environment, listen on `127.0.0.1:$PORT`, and run fine without the host too.

`kit/` is a separate Go module with helpers:

- `kit.Load(appID, defaultPort)`: `PORT`, `DATA_DIR`, and the same guest/persistent pattern as the host when started by hand.
- `kit.Storage` with `LocalStorage` and `DriveStorage` (keeps data in `Apps/<app-id>/` in Drive). Most apps don't need this: choosing files with **Files** (or in the app's cloud settings) lets the host sync them.
- `kit.Snapshots`: timestamped copies, keep the last N, and `RestoreIfNewer` for a startup check.
- `kit.LoopbackLogin` and `kit.FileTokenSource` for the app's own Google sign-in.

Build your apps as single executables (UI embedded, pure-Go SQLite) so they run on computers without Go or Node, and publish releases with `docs/app-release-workflow.yml`.

## Releases

Push a tag like `v0.1.0`. `.github/workflows/release.yml` builds the UI, runs the tests, cross-compiles for Windows, macOS and Linux (amd64 and arm64) and attaches zip files to a GitHub release.

## Layout

```
cmd/mydailyapps/     entry point: config, state file, server, shutdown path
internal/config/     env + mydailyapps.env, guest/persistent folders
internal/state/      local SQLite: app paths, last launched, approvals, settings, Drive token
internal/library/    library file model, validation, local cache
internal/drive/      Google OAuth (loopback redirect, PKCE) and Drive file calls, app folders and backups
internal/install/    folder scan, requirement checks, git clone, release download/unpack, updates
internal/cloudsync/  Steam-style file sync of app data (pull before start, push after exit)
internal/detect/     drafts a library entry from a repository with fixed rules
internal/observe/    snapshots of what an app writes, keep/temporary suggestions
internal/procs/      launch, ready check, stop, tree kill, state file (procs_windows.go, procs_unix.go)
internal/host/       ties it together (used by the HTTP layer)
internal/api/        HTTP handlers and the embedded UI
ui/                  React + TypeScript (Vite)
kit/                 shared Go module for your own apps
```

## Notes on choices

- SQLite uses `github.com/ncruces/go-sqlite3`, a pure-Go driver (no C compiler needed), like the planned `modernc.org/sqlite`.
- Drive is called through its REST API with `golang.org/x/oauth2` for sign-in, instead of the large `google.golang.org/api` client. The host needs only a handful of calls.
- The HTTP API listens on `127.0.0.1` only, rejects requests whose `Host` is not localhost (DNS rebinding), and requires an `X-MyDailyApps: 1` header on every change (cross-site requests cannot add it).
