# Plan: homelab-backup service (Go + HTMX + restic)

> Target repo for the service implementation: `/home/blaine/work/homelab-backup`
> (copy/move this plan there when switching to build mode)

## Decisions

| Area | Choice |
|---|---|
| Engine | restic snapshots, retention keep-daily 7 / keep-weekly 4 |
| Offsite | nightly rsync of the restic repo to another host/NAS |
| Scheduling | systemd user timers (generated from the YAML's cron schedules; no crontab) |
| App | Go + HTMX single binary, separate repo, dashboard on port ~3095 |
| Config | `backup-services.yaml` in the homelab repo is the single source of truth |

## Configuration loading (via .env)

The service derives **all source config paths from its own .env file** — no hard-coded paths:

- Service env file lives in the repo: `/home/blaine/work/homelab-backup/homelab-backup.env`
- Example contents:

```
HOMELAB_BACKUP_CONFIG=/home/blaine/homelab/services/homelab-backup/backup-services.yaml
HOMELAB_DATA_ROOT=/home/blaine/homelab-data
HOMELAB_BACKUP_STATE=/home/blaine/homelab-data/backups/state
HOMELAB_CONTAINER_BASE_DIR=/home/blaine/homelab/services
# per-service env files (credentials) are referenced FROM the yaml,
# so the yaml lists e.g. env_file: /home/blaine/homelab/services/sure/sure.env
OFFSITE_HOST=<TBD>
OFFSITE_PATH=<TBD>
```

- `homelab-backup.service` (systemd user unit) uses `EnvironmentFile=` to load it;
  the dashboard also exposes `/status/config` showing the resolved paths.
- Postgres credentials are pulled per-procedure from each service's own .env
  (e.g. `/home/blaine/homelab/services/sure/sure.env`,
  `sparkyfitness.env`) — the yaml declares the path, never the password.
  Credentials are never committed to git.

## Part 1 — Service declaration (in `homelab` repo)

New `services/homelab-backup/backup-services.yaml`:

```yaml
defaults:
  restic_repo: ${HOMELAB_DATA_ROOT}/backups/restic
  retention: { keep_daily: 7, keep_weekly: 4 }

services:
  sparkyfitness:
    env_file: ${HOMELAB_CONTAINER_BASE_DIR}/sparkyfitness/sparkyfitness.env
    procedures:
      - id: postgres          # podman exec sparkyfitness-db pg_dumpall -U sparky | gzip
        db_user: sparky
        db_password_env: POSTGRES_PASSWORD
        schedule: "0 2 * * *"
      - id: uploads           # tar of ${HOMELAB_DATA_ROOT}/sparkyfitness/uploads
        schedule: "30 2 * * *"
  sure:
    env_file: ${HOMELAB_CONTAINER_BASE_DIR}/sure/sure.env
    procedures:
      - id: postgres          # podman exec sure-db pg_dumpall -U sure_user
        db_user: sure_user
        db_password_env: POSTGRES_PASSWORD
        schedule: "0 3 * * *"
      - id: rails_storage     # ${HOMELAB_DATA_ROOT}/sure/rails
        schedule: "30 3 * * *"
      # redis intentionally ignored (matches umbrel backupIgnore intent)
  openwebui:
    env_file: ${HOMELAB_CONTAINER_BASE_DIR}/openwebui/openwebui.env
    procedures:
      - id: data              # ${HOMELAB_DATA_ROOT}/openwebui/data (4.5 GB, restic dedups)
        schedule: "0 4 * * *"
  searxng:
    env_file: ${HOMELAB_CONTAINER_BASE_DIR}/searxng/searxng.env
    procedures:
      - id: config            # ${HOMELAB_DATA_ROOT}/searxng/config (tiny)
        schedule: "30 1 * * 0"
```

Env-var expansion (`${VAR}` from the service .env) applied by the Go config loader.

## Part 2 — Go + HTMX service (`~/work/homelab-backup`)

```
homelab-backup/
├── cmd/homelab-backup/          # main: dashboard server + `--run <procedure>` mode
├── internal/
│   ├── config/                  # .env loading, yaml parse, ${VAR} expansion
│   ├── runner/                  # restic / podman exec pg_dump / tar execution
│   ├── retention/               # restic forget after runs
│   ├── offsite/                 # rsync push (lock-guarded)
│   └── store/                   # sqlite run history
├── web/                         # HTMX templates (status grid, run-now, history)
├── homelab-backup.env           # all paths live here
└── systemd/
    ├── homelab-backup.service   # EnvironmentFile=homelab-backup.env
    └── timer generation         # OnCalendar= derived from yaml cron rules
```

- Runs as a **host systemd user service** (not a container) — needs `podman exec`,
  restic, rsync on the host. Systemd timers are regenerated when the yaml changes.
- Dashboard (port 3095): per-service status grid, "Run now" (HTMX POST),
  history with sizes/results, restore hints.
- Agent runs described by timers invoke `homelab-backup --run <procedure-id>`.

## Part 3 — Rollout steps

1. Install prerequisites: Go toolchain, restic.
2. Scaffold Go repo; implement config loader (.env → yaml paths); build & test each procedure manually.
3. Write systemd user service + timer generation; dashboard UI.
4. Deploy `backup-services.yaml` in homelab repo; README with restore instructions
   (`restic restore` for files, `psql` restore from dumps).
5. Choose offsite host/path, enable rsync push timer.
6. When back in the homelab repo: require new services to add entries to
   backup-services.yaml as part of their setup.

## Open items

- Offsite destination host/path (`OFFSITE_HOST`, `OFFSITE_PATH`).
- Distro name (for prerequisite install commands).
- Port choice for the dashboard (default 3095).
