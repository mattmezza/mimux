# Migrating from `sm` to `mimux`

The project was renamed from **sm** to **mimux** in v0.20. Nothing about your
mail changes — the database, accounts, credentials and OAuth tokens all carry
over. What changes is the names of things around it.

Most of this is automatic. The one item you must do by hand is the **image
path**; everything else has a shim or fixes itself on first boot.

The compose service is also renamed (`sm:` → `mimux:`), which changes the
container name and its network alias. If a reverse proxy in front of it
addresses the container by name, update that too.

## 1. Docker image

`ghcr.io/mattmezza/sm` → `ghcr.io/mattmezza/mimux`

Update your `docker-compose.yml` (or `docker run`) to pull the new image. The
old tags stay where they are; they simply stop getting new releases.

## 2. Environment variables: `SM_` → `MIMUX_`

| Old | New |
|-----|-----|
| `SM_DB` | `MIMUX_DB` |
| `SM_HOST` | `MIMUX_HOST` |
| `SM_PORT` | `MIMUX_PORT` |
| `SM_BASE_URL` | `MIMUX_BASE_URL` |
| `SM_SECRET` | `MIMUX_SECRET` |
| `SM_AI_BASE_URL` | `MIMUX_AI_BASE_URL` |

**Deprecation window:** the `SM_` names still work in v0.20. Each one that is
read logs a warning naming the old and the new variable:

```
level=WARN msg="config: deprecated env var, rename it" old=SM_DB new=MIMUX_DB
```

`MIMUX_*` wins when both are set. **The `SM_` fallback is removed in v0.21** —
rename them before then.

## 3. Database file: `sm.db` → `mimux.db`

The default path moved from `./data/sm.db` to `./data/mimux.db` (and
`/data/sm.db` → `/data/mimux.db` in Docker).

**This is handled for you.** On startup, if the new file is absent and the old
one is present, mimux renames `sm.db` and its `-wal`/`-shm` sidecars in place
and logs it:

```
level=INFO msg="renamed legacy database from the sm -> mimux rename" from=/data/sm.db to=/data/mimux.db
```

The rename is fatal if it fails, rather than quietly booting onto an empty
database next to your populated one. Everything lives in that single file —
message bodies, attachments, the search index, sessions, VAPID keys — so there
is nothing else to move.

Two caveats:

- If you set `MIMUX_DB` (or `SM_DB`) to a path whose filename doesn't contain
  `mimux`, no migration is attempted — your explicit path is used as-is.
- The `secret` file next to the database is unchanged and keeps its name.

**Back up your data directory before the first boot on v0.20.** The rename is
straightforward, but it is still a one-way move over live data.

## 4. Docker volume

The stock `docker-compose.yml` uses a bind mount (`./data:/data`), which is
unaffected — the directory keeps its name and its contents.

If you replaced it with a *named* volume called `sm_data` (or similar) and you
rename it to `mimux_data`, Docker will create a new, empty volume and **orphan
the old one** — your mail will appear to be gone. Either keep the old volume
name, or copy the contents across first:

```sh
docker volume create mimux_data
docker run --rm -v sm_data:/from -v mimux_data:/to alpine \
  sh -c 'cp -a /from/. /to/'
```

Then remove `sm_data` once you have confirmed the new container boots with your
mail intact.

## 5. You will be signed out

The session and CSRF cookies were renamed (`sm_session` → `mimux_session`,
`sm_csrf` → `mimux_csrf`, `sm_oauth_state` → `mimux_oauth_state`). Every active
session is therefore invalid on first boot — a clean break now rather than a
second forced logout later.

Sign in again with the same admin password. Your account is untouched; only the
browser cookie is.

## 6. Other one-time resets

None of these lose data, but you will notice them:

- **Browser-local UI preferences reset.** Theme, list width, the last-open
  settings tab and per-message light/dark overrides were stored under `sm.*`
  keys in `localStorage` and are now `mimux.*`. They fall back to defaults once.
- **The service worker cache was bumped** (`sm-v7` → `mimux-v8`) so installed
  PWAs evict the old assets instead of serving a half-renamed app. The first
  load after upgrading refetches CSS/JS.
- **The PWA shows the new name** ("mimux") after the browser refreshes the
  manifest. The manifest `start_url` is unchanged, so an installed app keeps its
  identity and updates in place rather than becoming a second install.
- **Backup export filename** changed from `sm-config.json` to
  `mimux-config.json`. Existing exports still import fine — the filename is not
  part of the format.

## 7. Visible in outgoing mail

Two identifiers that recipients can see were renamed. Nothing to do; noted for
completeness:

- The stylesheet class scoping rich-text message bodies (`.sm-body` →
  `.mimux-body`).
- The iCalendar `PRODID` on invites we generate (`-//SM//Calendar//EN` →
  `-//mimux//Calendar//EN`).

## Checklist

```
[ ] Back up ./data (or your volume)
[ ] Point the image at ghcr.io/mattmezza/mimux
[ ] Rename SM_* env vars to MIMUX_* (incl. MIMUX_DB=/data/mimux.db)
[ ] Boot, confirm the "renamed legacy database" log line
[ ] Sign in again
[ ] Check mail is all there before deleting any backup
```
