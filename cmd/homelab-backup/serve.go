// HTMX dashboard + endpoints: status grid, run-now, snapshot browsing,
// backup downloads, and the settings pages (root settings, rsync targets,
// backups editor).
package main

import (
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"homelab-backup/internal/config"
	"homelab-backup/internal/offsite"
	"homelab-backup/internal/runner"
	"homelab-backup/internal/schedule"
	"homelab-backup/internal/store"
	"homelab-backup/internal/web"
)

type runningTracker struct {
	mu       sync.Mutex
	inflight map[string]bool
	svcLocks map[string]*sync.Mutex
}

var tracker = runningTracker{inflight: map[string]bool{}, svcLocks: map[string]*sync.Mutex{}}

type row struct {
	Service    string
	Procedure  string
	Type       string
	Logo       string // /logo/<service> or empty for generic
	HasLogo    bool
	Schedule   string
	NextRun    string
	Latest     *store.Run
	Running    bool
	SchedError error
}

type navData struct{ Current string }

func serve(envPath string) {
	env, err := config.LoadEnv(envPath)
	if err != nil {
		fatal(err)
	}
	reload := func() (*config.Config, error) {
		return config.Load(env.Values, env.BackupConfigPath)
	}

	h := http.NewServeMux()

	h.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		cfg, cfgErr := reload()
		var rows []row
		if cfgErr == nil {
			rows = buildRows(env, cfg)
		}
		render(w, "index.html", map[string]any{
			"Rows":        rows,
			"Port":        env.Port,
			"EnvPath":     env.EnvPath,
			"ConfigPath":  env.BackupConfigPath,
			"OffsiteHost": offsiteHost(env, cfg),
			"CfgError":    errText(cfgErr),
			"Flash":       flashRead(w, r),
			"Nav":         navData{Current: "/"},
		})
	})

	// Per-service logos, resolved against the embedded asset set with an
	// optional per-service override (service.logo = file path on disk).
	h.HandleFunc("GET /logo/{svc}", func(w http.ResponseWriter, r *http.Request) {
		svcName := r.PathValue("svc")
		if cfg, err := reload(); err == nil {
			if svc, ok := cfg.Services[svcName]; ok && svc.Logo != "" {
				if b, err := os.ReadFile(svc.Logo); err == nil {
					w.Header().Set("Content-Type", mimeOf(svc.Logo))
					w.Write(b)
					return
				}
			}
		}
		for _, ext := range []string{".svg", ".png"} {
			asset := "assets/" + svcName + ext
			if web.HasAsset(asset) {
				f, err := web.Asset(asset)
				if err == nil {
					defer f.Close()
					w.Header().Set("Content-Type", mimeOf(asset))
					io.Copy(w, f)
					return
				}
			}
		}
		http.NotFound(w, r)
	})

	h.HandleFunc("POST /run/{svc}/{proc}", func(w http.ResponseWriter, r *http.Request) {
		cfg, cfgErr := reload()
		if cfgErr != nil {
			http.Error(w, "config error: "+cfgErr.Error(), 500)
			return
		}
		svcName, procID := r.PathValue("svc"), r.PathValue("proc")
		svc, ok := cfg.Services[svcName]
		if !ok {
			http.Error(w, "unknown service", 404)
			return
		}
		var proc *config.Procedure
		for _, p := range svc.Procedures {
			if p.ID == procID {
				proc = p
			}
		}
		if proc == nil {
			http.Error(w, "unknown procedure", 404)
			return
		}
		mu := procLock(svcName, procID)
		mu.Lock()
		inflight := tracker.inflight[svcName+"/"+procID]
		if !inflight {
			tracker.inflight[svcName+"/"+procID] = true
		}
		mu.Unlock()
		if inflight {
			w.WriteHeader(200)
			renderRows(w, env, cfg, svcName, procID)
			return
		}
		go func(s *config.Service, p *config.Procedure) {
			defer func() {
				mu.Lock()
				delete(tracker.inflight, svcName+"/"+procID)
				mu.Unlock()
			}()
			_ = runner.Run(env, cfg, svcName, s, p)
		}(svc, proc)
		w.WriteHeader(200)
		renderRows(w, env, cfg, svcName, procID)
	})

	// ---- snapshots & downloads ----

	h.HandleFunc("GET /snapshots/{svc}/{proc}", func(w http.ResponseWriter, r *http.Request) {
		svcName, procID := r.PathValue("svc"), r.PathValue("proc")
		procType := ""
		if cfg, err := reload(); err == nil {
			if svc, ok := cfg.Services[svcName]; ok {
				for _, p := range svc.Procedures {
					if p.ID == procID {
						procType = p.Type
					}
				}
			}
		}
		cfg, cfgErr := reload()
		if cfgErr != nil {
			cfg = minimalCfg(env) // downloads still work off the env's repo
		}
		snaps, err := runner.Snapshots(env, cfg, svcName, procID)
		var browse []runner.LsEntry
		var browseErr string
		if r.URL.Query().Get("browse") == "1" && err == nil && len(snaps) > 0 {
			snap := snaps[0]
			if b, e := runner.ResolveSnapshot(env, cfg, svcName, procID, snap.Short()); e == nil {
				entries, le := runner.TopEntries(env, cfg, b.ID)
				if le != nil {
					browseErr = le.Error()
				}
				browse = entries
			} else {
				browseErr = e.Error()
			}
		}
		data := map[string]any{
			"Nav": navData{Current: "/snapshots"},
			"Port": env.Port, "EnvPath": env.EnvPath, "ConfigPath": env.BackupConfigPath,
			"Svc": svcName, "Proc": procID, "ProcType": procType,
			"Snaps": snaps,
			"Browse": browse != nil, "Entries": browse, "BrowseErr": browseErr,
			"SnapErr": errText(err), "Flash": flashRead(w, r),
		}
		render(w, "snapshots.html", data)
	})

	h.HandleFunc("GET /download/{svc}/{proc}", func(w http.ResponseWriter, r *http.Request) {
		svcName, procID := r.PathValue("svc"), r.PathValue("proc")
		if svcName == "offsite" {
			errPage(w, "offsite sync runs have nothing to download", http.StatusNotFound)
			return
		}
		cfg, cfgErr := reload()
		if cfgErr != nil {
			errPage(w, "config error: "+cfgErr.Error(), http.StatusInternalServerError)
			return
		}
		procType := ""
		if svc, ok := cfg.Services[svcName]; ok {
			for _, p := range svc.Procedures {
				if p.ID == procID {
					procType = p.Type
				}
			}
		}
		if procType == "" {
			errPage(w, fmt.Sprintf("unknown procedure %s/%s", svcName, procID), http.StatusNotFound)
			return
		}
		snap, err := runner.ResolveSnapshot(env, cfg, svcName, procID, r.URL.Query().Get("snap"))
		if err != nil {
			errPage(w, err.Error(), http.StatusNotFound)
			return
		}
		file := r.URL.Query().Get("file")
		if file != "" {
			streamDump(w, r, env, cfg, snap, file)
			return
		}
		if procType == "postgres_dump" {
			streamDump(w, r, env, cfg, snap, svcName+"-"+procID+".sql.gz")
			return
		}
		// whole snapshot as tar.gz
		name := sanitizeFile(fmt.Sprintf("%s-%s-%s.tar.gz", svcName, procID, snap.Short()))
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		if err := runner.DownloadTarGz(env, cfg, w, svcName, procID, snap); err != nil {
			log.Printf("download %s/%s: %v", svcName, procID, err)
		}
	})

	// Per-app page: procedures + full-run history for one service.
	h.HandleFunc("GET /service/{svc}", func(w http.ResponseWriter, r *http.Request) {
		svcName := r.PathValue("svc")
		cfg, cfgErr := reload()
		if cfgErr != nil {
			errPage(w, "config error: "+cfgErr.Error(), http.StatusInternalServerError)
			return
		}
		svc, ok := cfg.Services[svcName]
		if !ok {
			errPage(w, "unknown service "+svcName, http.StatusNotFound)
			return
		}
		// rows scoped to this service
		all := buildRows(env, cfg)
		rows := all[:0]
		for _, rw := range all {
			if rw.Service == svcName {
				rows = append(rows, rw)
			}
		}
		history := serviceHistory(env, svcName, svc.Logo != "")
		hasLogo := web.HasAsset("assets/"+svcName+".svg") || web.HasAsset("assets/"+svcName+".png") || svc.Logo != ""
		render(w, "service.html", map[string]any{
			"Nav": navData{Current: "/service"},
			"Port": env.Port, "EnvPath": env.EnvPath, "ConfigPath": env.BackupConfigPath,
			"Name": svcName, "HasLogo": hasLogo,
			"Rows": rows, "History": history,
			"Flash": flashRead(w, r),
		})
	})

	// ---- settings routes ----

	h.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) { settingsRootHandler(env).ServeHTTP(w, r) })
	h.HandleFunc("POST /settings/env", func(w http.ResponseWriter, r *http.Request) { settingsEnvSave(w, r, env) })
	h.HandleFunc("POST /settings/defaults", func(w http.ResponseWriter, r *http.Request) { settingsDefaultsSave(w, r, env) })
	h.HandleFunc("POST /settings/offsite", func(w http.ResponseWriter, r *http.Request) { settingsOffsiteSave(w, r, env) })
	h.HandleFunc("POST /settings/target", func(w http.ResponseWriter, r *http.Request) { settingsTargetSave(w, r, env) })
	h.HandleFunc("POST /settings/target-delete", func(w http.ResponseWriter, r *http.Request) { settingsTargetDelete(w, r, env) })
	h.HandleFunc("GET /settings/backups", func(w http.ResponseWriter, r *http.Request) { backupsHandler(env).ServeHTTP(w, r) })
	h.HandleFunc("POST /settings/backups/service", func(w http.ResponseWriter, r *http.Request) { backupsServiceSave(w, r, env) })
	h.HandleFunc("POST /settings/backups/service-delete", func(w http.ResponseWriter, r *http.Request) { backupsServiceDelete(w, r, env) })
	h.HandleFunc("POST /settings/backups/proc", func(w http.ResponseWriter, r *http.Request) { backupsProcSave(w, r, env) })
	h.HandleFunc("POST /settings/backups/proc-delete", func(w http.ResponseWriter, r *http.Request) { backupsProcDelete(w, r, env) })
	h.HandleFunc("POST /settings/backups/timers", func(w http.ResponseWriter, r *http.Request) { backupsTimers(w, r, env) })

	// Push the restic repo to all rsync targets now (-background).
	var pushMu sync.Mutex
	h.HandleFunc("POST /settings/offsite-push", func(w http.ResponseWriter, r *http.Request) {
		pushMu.Lock()
		busy := tracker.inflight["__offsite__"]
		if !busy {
			tracker.inflight["__offsite__"] = true
		}
		pushMu.Unlock()
		if busy {
			flashSet(w, "An offsite push is already running")
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		cfg, cfgErr := reload()
		if cfgErr != nil {
			tracker.inflight["__offsite__"] = false
			badFlash(w, r, "/settings", "config error: "+cfgErr.Error())
			return
		}
		go func(cfg *config.Config) {
			defer func() { tracker.inflight["__offsite__"] = false }()
			msg, err := offsite.Push(env, cfg)
			fmt.Println("offsite push:", msg)
			if err != nil {
				fmt.Fprintln(os.Stderr, "ERROR:", err)
			}
		}(cfg)
		flashSet(w, "Offsite push started in the background — see the recent pushes table when it finishes")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	})

	addr := env.Addr + ":" + env.Port
	if env.Addr == "" {
		addr = "127.0.0.1:" + env.Port
	}
	srv := &http.Server{Addr: addr, Handler: logHandler(h), ReadHeaderTimeout: 10 * time.Second}
	srv.ListenAndServe()
}

// serviceHistory returns the newest runs of one service (history page).
func serviceHistory(env *config.Env, svcName string, _ bool) []store.Run {
	all, _, err := store.Load(env.StateDir, 0)
	if err != nil {
		return nil
	}
	var out []store.Run
	for _, r := range all {
		if r.Service == svcName {
			out = append(out, r)
			if len(out) >= 100 {
				break
			}
		}
	}
	return out
}

func errPage(w http.ResponseWriter, msg string, code int) {
	render(w, "error.html", map[string]any{
		"Nav": navData{Current: ""},
		"Msg": msg,
	})
}

// streamDump pipes a single file (or db dump) out of the snapshot. The
// first chunk is read before headers are sent so early restic failures
// still produce a proper error page.
func streamDump(w http.ResponseWriter, r *http.Request, env *config.Env, cfg *config.Config, snap runner.SnapInfo, file string) {
	base := filepath.Base(strings.TrimPrefix(file, "/"))
	contentType := "application/octet-stream"
	if strings.HasSuffix(base, ".gz") || strings.HasSuffix(base, ".tgz") {
		contentType = "application/gzip"
	}
	ds, err := runner.StartDump(env, cfg, snap.ID, file)
	if err != nil {
		errPage(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf := make([]byte, 32*1024)
	var chunk []byte
	for len(chunk) == 0 {
		n, rerr := ds.Read(buf)
		chunk = append(chunk, buf[:n]...)
		if rerr != nil {
			// stream ended before any content: surface the real error
			werr := ds.Wait()
			if werr == nil && len(chunk) == 0 {
				werr = fmt.Errorf("restic dump produced no data")
			}
			if rerr == io.EOF && len(chunk) > 0 {
				break // small dump: whole file already read
			}
			errPage(w, errJoin(rerr, werr), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFile(file)+`"`)
	w.Write(chunk)
	if _, err := io.Copy(w, ds); err != nil {
		log.Printf("download stream: %v", err)
	}
	if err := ds.Wait(); err != nil {
		log.Printf("restic dump: %v", err)
	}
}

func errJoin(errs ...error) string {
	var msgs []string
	for _, e := range errs {
		if e != nil {
			msgs = append(msgs, e.Error())
		}
	}
	return strings.Join(msgs, "\n")
}

func logHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("%s %s\n", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func procLock(svc, proc string) *sync.Mutex {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	key := svc + "/" + proc
	if tracker.svcLocks[key] == nil {
		tracker.svcLocks[key] = &sync.Mutex{}
	}
	return tracker.svcLocks[key]
}

func buildRows(env *config.Env, cfg *config.Config) []row {
	_, latest, err := store.Load(env.StateDir, 0)
	if err != nil {
		fatal(err)
	}
	var rows []row
	for svcName, svc := range cfg.Services {
		for _, proc := range svc.Procedures {
			r := row{Service: svcName, Procedure: proc.ID, Type: proc.Type, Schedule: proc.Schedule}
			r.Logo = "/logo/" + svcName
			r.HasLogo = web.HasAsset("assets/"+svcName+".svg") || web.HasAsset("assets/"+svcName+".png") || svc.Logo != ""
			if c, err := schedule.Parse(proc.Schedule); err == nil {
				r.NextRun = c.Next(time.Now()).Format(time.RFC1123)
			} else {
				r.SchedError = err
			}
			if lw, ok := latest[svcName+"/"+proc.ID]; ok {
				lr := lw
				r.Latest = &lr
			}
			tracker.mu.Lock()
			r.Running = tracker.inflight[svcName+"/"+proc.ID]
			tracker.mu.Unlock()
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Service < rows[j].Service })
	return rows
}

func offsiteHost(env *config.Env, cfg *config.Config) string {
	if cfg != nil && cfg.Offsite.Host != "" {
		return cfg.Offsite.Host
	}
	return env.OffsiteHost
}

func renderRows(w http.ResponseWriter, env *config.Env, cfg *config.Config, svc, proc string) {
	rows := buildRows(env, cfg)
	var sel []row
	for _, r := range rows {
		if r.Service == svc && r.Procedure == proc {
			sel = append(sel, r)
		}
	}
	render(w, "rowlist", sel)
}

// render loads templates from disk (template dir beside the binary) and falls
// back to compiled-in copies under the homelab-backup service.
func render(w http.ResponseWriter, name string, data any) {
	t, err := loadTemplates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "template exec error: "+fmt.Sprintf("%v", err), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(buf.String()))
}

func loadTemplates() (*template.Template, error) {
	funcs := template.FuncMap{"fmtBytes": fmtBytes, "upper": upperAny}
	for _, d := range []string{os.Getenv("HOMELAB_BACKUP_TEMPLATES"), "web"} {
		if d == "" {
			continue
		}
		if t, err := template.New("").Funcs(funcs).ParseGlob(filepath.Join(d, "*.html")); err == nil {
			return t, nil
		}
	}
	if t, err := template.New("").Funcs(funcs).ParseFS(web.FS, "*.html"); err == nil {
		return t, nil
	}
	return nil, fmt.Errorf("templates not found")
}

// upperAny makes the fallback first-letter render safe when `index` yields a rune.
func upperAny(v any) string { return strings.ToUpper(fmt.Sprint(v)) }

// mimeOf maps an asset filename extension to a content type.
func mimeOf(name string) string {
	switch {
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

// fmtBytes renders byte counts in human units.
func fmtBytes(b int64) string {
	if b <= 0 {
		return ""
	}
	const kb, mb, gb = 1024, 1024 * 1024, 1024 * 1024 * 1024
	switch {
	case b > gb:
		return fmt.Sprintf("%.1f GB", float64(b)/gb)
	case b > mb:
		return fmt.Sprintf("%.1f MB", float64(b)/mb)
	default:
		return fmt.Sprintf("%.1f KB", float64(b)/kb)
	}
}

var nameCleanRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFile keeps only safe characters for download filename headers.
func sanitizeFile(s string) string {
	cleaned := nameCleanRe.ReplaceAllString(s, "_")
	return strings.Trim(cleaned, "_")
}
