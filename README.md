# homelab-backup

Standalone backup agent + dashboard for the homelab (source config: the
`backup-services.yaml` in the `homelab` repo). Go + HTMX, restic engine.

## How it fits together

```
homelab repo
└── services/homelab-backup/backup-services.yaml   ← WHAT to back up, when, how (source of truth)

this repo
├── homelab-backup.env      ← WHERE paths live (config path, data root, state dir, offsite target)
├── cmd/…                    ← agent: run / run-scheduled / install-timers / offsite / serve
├── internal/{config,runner,offsite,store,schedule}
├── web/                     ← HTMX dashboard (embedded, overridable on disk)
└── systemd/homelab-backup.service
```

The agent derives **all** source paths from `homelab-backup.env`
(`HOMELAB_BACKUP_CONFIG`, `HOMELAB_DATA_ROOT`, `HOMELAB_BACKUP_STATE`,
`HOMELAB_CONTAINER_BASE_DIR`). Changing repo location = edit the .env only.

Service credentials (DB passwords) live in each service's own env file in the
homelab repo, referenced from the yaml — never committed here.

## Setup

```sh
# toolchain (user-space ok)
#   go    → ~/.local/go  (https://go.dev/dl)
#   restic → ~/.local/bin (https://github.com/restic/restic)

cp .env.example homelab-backup.env   # edit paths
go build -o bin/homelab-backup ./cmd/homelab-backup

bin/homelab-backup init                          # init restic repo (password auto-generated)
bin/homelab-backup run <service> <procedure>     # test one procedure
bin/homelab-backup serve                         # dashboard on 127.0.0.1:3095

# scheduled runs: generate + enable systemd user timers from the yaml
bin/homelab-backup install-timers
systemctl --user list-timers | grep homelab-backup

# install the dashboard service
cp systemd/homelab-backup.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now homelab-backup
```

## Running backups

- **Manual single**  `bin/homelab-backup run sparkyfitness postgres`
- **Scheduled sweep** (what the timers invoke)  `bin/homelab-backup run-scheduled`
  - runs every procedure whose cron schedule fired since its last success
  - piggybacks the nightly offsite rsync push (gated, one per OFFSITE_MIN_HOURS)
- **Dashboard** `POST /run/<service>/<procedure>` ("Run now" button)

## Dashboard pages

- `/` — status grid: latest run per procedure, next scheduled run, "Run now",
  per-app pages and Snapshot/Download links.
- `/service/<service>` — one app: its procedures plus full run history
  (state dir runs.jsonl), with download links per historical snapshot.
- `/snapshots/<service>/<procedure>` — restic snapshots for a procedure;
  download a whole snapshot (tar.gz), a file, or a db dump (via `restic dump`).
  Whole-snapshot downloads restore to a temp dir under the state dir first —
  mind free disk space.
- `/settings/backups` — edit the configured services & procedures on file:
  schedules, paths, containers/db user, add/remove/rename, plus a timers
  reinstall button. Writes are validated and atomic; ${VAR} references are
  preserved. Saving regenerates the systemd timers.
- `/settings` — root-level settings (the agent .env keys: data root, state
  dir, ports, offsite fallbacks), the yaml top-level defaults (restic repo +
  retention) and the offsite section: master switch, legacy primary host/path
  and a list of rsync targets (pushes fan out to each enabled target) with a
  "push now" button and recent-push results.

## Restores

Files (restic restores any snapshot):

```sh
export RESTIC_REPOSITORY=… RESTIC_PASSWORD_FILE=~/.config/… or $HOMELAB_BACKUP_STATE/restic-pass
restic snapshots --tag service:openwebui
restic restore <id> --target /tmp/restore
```

Postgres dumps (piped via stdin; filenames `<service>-<procedure>.sql.gz`):

```sh
restic dump latest <svc>-<proc>.sql.gz | gunzip | \
  podman exec -i -e PGPASSWORD=… <db-container> psql -U <user> -d postgres
```

## Logos

The dashboard resolves each service's icon as follows:
1. `logo:` in `backup-services.yaml` — a direct URL or a local path
2. the service's `app.yaml` `icon:` field (same convention Homeio uses for its
   app tiles, so both dashboards share one value; define it once)
3. built-in embedded icons, then a letter badge

Remote icons are downloaded once and cached in `<STATE_DIR>/logos/` for 24h.

## Adding a service

Add a block to `backup-services.yaml` (env_file + procedures + schedules),
then `bin/homelab-backup install-timers` to regenerate timers. That is the
entire workflow — see PLAN.md for the original design plan.
