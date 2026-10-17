# Initiative Tracker: Implementation Plan

A TTRPG initiative tracker that runs as a MyDailyApps app (group `D&D`). One page, black and green, built for use at the table during combat.

## 1. Settled decisions

- **Frontend:** React + TypeScript (Vite), plain CSS, no component library, no router.
- **Backend:** small Node server in TypeScript (Express). It serves the built UI and saves encounters as JSON files under `$DATA_DIR`, so MyDailyApps can sync them to Google Drive. A browser alone cannot write files, which is why the server exists.
- **Install:** `git` option only (`npm ci`, then `npm start`). Every machine that runs it needs Node 20 or newer. No `release` option for now (see section 9).
- **Save flow:** a Save button writes `<encounter-name>.json`. An Open menu lists saved encounters to load, with New encounter and a (soft) delete.
- **Combat helpers:** turn tracking (active row, Prev/Next, round counter), Sort by initiative, Add/remove rows, condition durations in rounds.
- **Hitpoints:** one editable integer, like Initiative and AC.
- **Conditions:** the 15 standard D&D 5e conditions (SRD), shown as icons with the name on hover.
- **Icons:** [game-icons.net](https://game-icons.net) SVGs (free, CC BY 3.0, attribution required). Copied into the repo so the app works offline.

## 2. Look and layout

Simple black and green: near-black background, one green accent, light grey text. No gradients, no shadows beyond a faint border.

| Token | Value | Use |
|---|---|---|
| `--bg` | `#0b0d0b` | page |
| `--surface` | `#121612` | table rows |
| `--border` | `#1f2a1f` | row and input borders |
| `--text` | `#d7e3d7` | text |
| `--muted` | `#7d8f7d` | placeholders, header labels |
| `--accent` | `#3ddc84` | active row, focus ring, icons, primary button |
| `--danger` | `#e5534b` | HP at 0 or below, delete |

```
                    [  Default Encounter  ]            ← click to edit, centered
  Round 1   [◀ Prev] [Next ▶]   [Sort]  [+ Row]       [Open ▾] [New] [Save ●]

  Init │ Name                     │ Conditions          │ AC │ HP │
  ─────┼──────────────────────────┼─────────────────────┼────┼────┼──
▌  18  │ [🛡]  Thorin             │ (prone)(poisoned³)  │ 18 │ 42 │ ✕
   15  │ [👹]  Goblin 1           │ (+)                 │ 15 │  7 │ ✕
       │ [👹]                     │ (+)                 │    │    │ ✕
   ... (6 empty rows on a new encounter)
```

- **Encounter name:** top middle, large text. Click to edit; Enter or leaving the field commits; Escape cancels. An empty name falls back to `Default Encounter`.
- **Toolbar** under the name: round counter, Prev/Next turn, Sort, Add row on the left; Open, New, Save on the right. A dot on Save shows unsaved changes. Ctrl+S also saves.
- **Rows:** all fields are inline inputs that look like plain text until focused (green focus ring).
- **Active row:** green bar on the left edge and a slightly lighter background.
- **Name cell:** class icon button, a gap, then the name input. The icon defaults to the monster icon. Clicking it opens a small popover grid of class icons (each with its label as tooltip).
- **Conditions cell:** row of small icons, then a `+` button. `+` opens a popover with the 15 conditions as icon + name, and an optional "rounds" number. Hovering an icon shows a tooltip with the name (and "3 rounds left" if timed). Clicking an applied icon opens a mini menu: change rounds, remove. A duration badge shows as a small number on the icon's corner.
- **HP:** turns red at 0 or below.
- **Accessibility:** real `<button>` and `<input>` elements with labels (`aria-label="Initiative for Thorin"`), tooltips also exposed via `aria-label`/`title`, keyboard reachable popovers (Escape closes), status shown by shape and text as well as color, 40 px minimum touch targets on buttons.

## 3. Data model

Shared between client and server in `src/shared/types.ts`.

```ts
type ClassId =
  | "monster" | "npc"
  | "artificer" | "barbarian" | "bard" | "cleric" | "druid" | "fighter"
  | "monk" | "paladin" | "ranger" | "rogue" | "sorcerer" | "warlock" | "wizard";

type ConditionId =
  | "blinded" | "charmed" | "deafened" | "exhaustion" | "frightened"
  | "grappled" | "incapacitated" | "invisible" | "paralyzed" | "petrified"
  | "poisoned" | "prone" | "restrained" | "stunned" | "unconscious";

interface AppliedCondition {
  id: ConditionId;
  roundsLeft: number | null;   // null = until removed by hand
  level?: number;              // exhaustion only, 1–6
}

interface Combatant {
  id: string;                  // crypto.randomUUID()
  initiative: number | null;   // INT; empty allowed
  classIcon: ClassId;          // default "monster"
  name: string;
  conditions: AppliedCondition[];
  ac: number | null;
  hp: number | null;
}

interface Encounter {
  schemaVersion: 1;
  id: string;                  // stable across renames
  name: string;                // default "Default Encounter"
  round: number;               // starts at 1
  activeId: string | null;     // combatant whose turn it is
  rows: Combatant[];
  updatedAt: string;           // ISO time, set by the server on save
  deleted?: boolean;           // soft delete (see section 5)
}
```

- Integer fields: inputs use `inputMode="numeric"`, accept an optional leading minus, and store `null` when empty. Non-digits are rejected while typing.
- A row counts as **empty** when it has no name and no initiative. Empty rows are skipped by turn tracking and sink to the bottom on sort.
- Condition and class metadata (id, label, icon file) live in `src/shared/conditions.ts` and `src/shared/classes.ts`, so adding one later is a one-line change.

## 4. Combat behaviour

- **Sort:** stable sort, highest initiative first; empty initiative goes to the bottom; ties keep their current order (so the DM can resolve ties by adding/removing a row or editing initiative). The active row stays active (tracked by `activeId`, not position). Sort is a button, not automatic, so rows don't jump while typing.
- **Next turn:** moves `activeId` to the next non-empty row. Wrapping past the last row increments `round`. If nothing is active yet, Next activates the first non-empty row (round stays 1).
- **Prev turn:** the reverse; wrapping backwards decrements `round` (not below 1). Prev does not restore expired conditions.
- **Condition durations:** a timed condition ticks down by 1 at the **start of that combatant's turn** (when Next makes the row active). At 0 it is removed. Untimed conditions are never touched.
- **Exhaustion:** stacks as a level (1–6) shown as the badge number instead of rounds.
- **Add row:** appends an empty row (monster icon). **Remove row:** the ✕ button. If the removed row was active, the next row becomes active.
- **New encounter:** name `Default Encounter`, round 1, 6 empty rows, nothing active. Asks first if there are unsaved changes.
- **Reset combat** (small menu item next to the round counter): round back to 1, nothing active, durations kept. Optional; cheap to add.

## 5. Saving and loading

Files live in `$DATA_DIR/encounters/<slug>.json`, one file per encounter. One file each keeps MyDailyApps' "newer copy wins" sync from losing more than one encounter.

- **Slug:** lowercase the name, replace runs of anything other than `a-z0-9` with `-`, trim dashes, cut to 64 characters; empty becomes `encounter`. `Goblin Ambush!` → `goblin-ambush.json`.
- **Save:** `PUT /api/encounters/<slug>` with the whole encounter.
  - If the file does not exist, create it.
  - If it exists and has the **same `id`**, overwrite it.
  - If it exists with a **different `id`** (another encounter already has that name), the server answers `409`; the UI asks "Overwrite 'Goblin Ambush'?" and resends with `?overwrite=1`.
  - **Renaming** an encounter and saving writes a new file under the new name. The old file stays (it may be wanted as a template); delete it from the Open menu if not.
- **Open:** lists saved encounters (name, last saved time), newest first. Choosing one asks first if there are unsaved changes.
- **Delete:** sync does not carry deletions (a deleted file comes back from Drive), so delete sets `"deleted": true` inside the file and the list hides it.
- **Writes are atomic:** write `<slug>.json.tmp`, then rename over the real file, so a crash never leaves half a file.
- **Startup state:** the app opens a fresh Default Encounter. The last opened slug is remembered in `localStorage` (convenience only) and offered as "Continue <name>" in the Open menu.

## 6. Server

`src/server/index.ts`, about 150 lines.

| Route | Does |
|---|---|
| `GET /api/encounters` | List `{ slug, name, updatedAt }` for non-deleted files |
| `GET /api/encounters/:slug` | Return one encounter |
| `PUT /api/encounters/:slug` | Validate and save (409 rule above); sets `updatedAt` |
| `DELETE /api/encounters/:slug` | Soft delete |
| `GET /*` | Static files from `dist/client`, falling back to `index.html` |

- **Port and data (MyDailyApps contract):** listen on `127.0.0.1:${PORT}`, default `5180` when `PORT` is missing. `DATA_DIR` defaults to `./data` next to the app; create `encounters/` on startup.
- **Safety:** `:slug` must match `^[a-z0-9][a-z0-9-]{0,63}$` (no path traversal); JSON body limit 1 MB; validate the shape (schemaVersion, arrays, integer fields) and reject anything else with 400.
- **Clean exit:** handle `SIGINT`, `SIGTERM` and `SIGBREAK` (Windows Ctrl+Break, which the host sends): stop accepting connections, then exit. Saves are atomic, so nothing is lost if the host force-kills after 5 seconds.
- **Startup log:** print `Initiative Tracker on http://127.0.0.1:<port>` and the data folder.

## 7. Repo layout

```
initiative-tracker/
  mydailyapps.json            manifest for the MyDailyApps host
  package.json, package-lock.json   (lock file must be committed: npm ci needs it)
  tsconfig.json               client
  tsconfig.server.json        server → dist/server
  vite.config.ts              builds client → dist/client; dev proxy /api → server
  index.html
  src/
    shared/   types.ts, conditions.ts, classes.ts, slug.ts
    server/   index.ts, storage.ts, validate.ts
    client/
      main.tsx, App.tsx, theme.css
      state/  encounterReducer.ts (all actions), useEncounterApi.ts
      components/
        EncounterTitle.tsx, Toolbar.tsx, CombatTable.tsx, CombatRow.tsx,
        IntField.tsx, ClassPicker.tsx, ConditionCell.tsx, ConditionPicker.tsx,
        Tooltip.tsx, Popover.tsx, OpenMenu.tsx, ConfirmDialog.tsx
      assets/icons/classes/*.svg, assets/icons/conditions/*.svg
  CREDITS.md                  game-icons.net attribution (author per icon, CC BY 3.0)
  README.md
```

`package.json` scripts:

```json
{
  "scripts": {
    "dev": "concurrently \"vite\" \"tsx watch src/server/index.ts\"",
    "build": "vite build && tsc -p tsconfig.server.json",
    "prepare": "npm run build",
    "start": "node dist/server/index.js",
    "test": "vitest run"
  },
  "engines": { "node": ">=20" }
}
```

`prepare` runs automatically after `npm ci`, so the host's single `setup` command installs **and** builds, and the setup string stays free of `&&` chains. State lives in one `useReducer`; no state library.

## 8. MyDailyApps manifest

`mydailyapps.json` in the repo root, written per the `mydailyapps-manifest` skill:

```json
{
  "manifestVersion": 1,
  "id": "initiative-tracker",
  "name": "Initiative Tracker",
  "group": "D&D",
  "description": "Turn order, conditions, AC and HP for TTRPG encounters.",
  "install": [
    { "type": "git", "requires": ["node", "npm"], "setup": "npm ci", "launch": "npm start" }
  ],
  "cloud": {
    "include": ["encounters/*.json"],
    "maxSizeMB": 20
  }
}
```

- Reads `PORT`, so no `port` field. Opens at `/` (default `path`).
- No SQLite, so no `cloud.sqlite`. Temp files end in `.json.tmp` and don't match `*.json`, so they are never synced.
- Nothing secret or regenerable is under `DATA_DIR`.

**Library entry needs updating.** The current entry in MyDailyApps' `library.json` lists a `release` option first (`launch: "tracker"`) and `setup: "npm install"`. Library fields override the manifest, so the host would try a release that doesn't exist. Replace the entry with:

```json
{ "id": "initiative-tracker", "repo": "https://github.com/OWNER/initiative-tracker" }
```

## 9. Icons

Download the SVGs from game-icons.net (or the `@iconify-json/game-icons` npm package), recolor with `fill="currentColor"` so CSS tints them green, and list each icon's author in `CREDITS.md` (CC BY 3.0 requires credit; a small "Icons: game-icons.net" link in the page footer covers the UI side).

Candidate icons, to confirm by name while picking them in Phase 2:

| Class | Candidate | | Condition | Candidate |
|---|---|---|---|---|
| Monster (default) | `imp-laugh` / `orc-head` | | Blinded | `blindfold` |
| NPC | `cowled` | | Charmed | `charm` / `hearts` |
| Artificer | `gears` | | Deafened | `deaf` |
| Barbarian | `battle-axe` | | Exhaustion | `tired-eye` |
| Bard | `lyre` | | Frightened | `screaming` |
| Cleric | `holy-symbol` | | Grappled | `grab` |
| Druid | `oak-leaf` | | Incapacitated | `dizzy` |
| Fighter | `broadsword` | | Invisible | `invisible` |
| Monk | `fist` | | Paralyzed | `paralysis` |
| Paladin | `templar-shield` | | Petrified | `stone-bust` |
| Ranger | `bow-arrow` | | Poisoned | `poison-bottle` |
| Rogue | `hood` / `cloak-dagger` | | Prone | `falling` |
| Sorcerer | `fire-ray` | | Restrained | `chains` |
| Warlock | `warlock-eye` | | Stunned | `knockout` |
| Wizard | `pointy-hat` | | Unconscious | `sleepy` |

Condition names and the 15-condition list come from the D&D 5e SRD (CC BY 4.0), which is fine to use.

## 10. Phases

Each phase ends in something you can run and check.

### Phase 0: Scaffolding
- `npm create vite@latest` (React + TS), add Express, `tsx`, `concurrently`, `vitest`. Add the scripts above and `tsconfig.server.json`.
- Express serves `dist/client` and `GET /api/ping`; reads `PORT` and `DATA_DIR`; handles shutdown signals.
- **Done when:** `npm ci` then `npm start` serves the Vite starter page on `http://127.0.0.1:5180`, and Ctrl+C exits cleanly.

### Phase 1: Theme and static layout
- `theme.css` with the tokens in section 2. Encounter title, toolbar, table header and 6 rows with placeholder content.
- **Done when:** the page matches the layout sketch at desktop and tablet width.

### Phase 2: Icons
- Pick and download the class and condition SVGs, recolor, add `classes.ts`, `conditions.ts`, `CREDITS.md`, footer credit.
- **Done when:** a scratch page shows all 15 class icons and 15 condition icons in green.

### Phase 3: Editable rows and state
- `encounterReducer` with actions: set name, edit field, set class, add row, remove row, new encounter.
- `IntField` (integers only, empty → `null`), editable title with Enter/Escape behaviour, class picker popover.
- **Done when:** every field edits, the title falls back to `Default Encounter`, rows add and remove, a new encounter starts with 6 empty rows.

### Phase 4: Conditions
- Condition cell, `+` picker with optional rounds, tooltip on hover/focus, remove/change menu, exhaustion level.
- **Done when:** conditions can be added, show their name on hover, show a rounds badge, and be removed.

### Phase 5: Combat helpers
- Sort, Next/Prev turn, round counter, active row style, duration tick at turn start, reset combat.
- Unit tests (vitest) for the reducer: sort with ties and empty rows, wrap-around round change, duration tick and removal, removing the active row.
- **Done when:** tests pass and a mock combat of 3 rounds behaves as described in section 4.

### Phase 6: Save and Open
- Server routes, slug, validation, atomic writes, 409 overwrite flow, soft delete.
- Client: Save button with unsaved dot, Ctrl+S, Open menu, delete, unsaved-changes confirm, last-opened in `localStorage`.
- Tests for slug, validation and the 409 rule.
- **Done when:** an encounter saves to `data/encounters/<slug>.json`, reloads identically after restarting the server, and deleted encounters disappear from the list but the file remains with `"deleted": true`.

### Phase 7: MyDailyApps integration
- Add `mydailyapps.json`, push the repo to GitHub, update the library entry as in section 8.
- **Done when:** in MyDailyApps the card shows Install; Install runs `npm ci` (which builds); Start opens the tracker in a tab; a saved encounter lands in the host's `DATA_DIR` for this app and syncs to Drive after Stop; on a second machine it comes back after Start.

### Phase 8: Polish
- Keyboard flow: Tab moves across a row then to the next row; Enter in Initiative jumps to Name.
- Empty state hints (placeholder text "Name", "—").
- README: how to run standalone (`npm ci`, `npm start`), and how to develop (`npm run dev`).

## 11. Not in scope for now

- Dice rolling, monster stat lookups, importing from D&D Beyond.
- Max HP, temp HP, damage/heal entry (HP is a single number by choice).
- Live sync between two open tabs or devices.
- A prebuilt `release` (single executable) install option. Possible later with Node's single executable apps feature, which would let the tracker run on machines without Node.
- Drag-and-drop row reordering (ties are resolved by editing initiative).
