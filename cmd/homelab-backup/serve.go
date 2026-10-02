// HTMX dashboard + endpoints.
package main

import (
		"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"io"
	"strings"
	"sync"
	"time"

	"homelab-backup/internal/config"
	"homelab-backup/internal/runner"
	"homelab-backup/internal/schedule"
	"homelab-backup/internal/store"
	"homelab-backup/internal/web"
)

type runningTracker struct {
	mu         sync.Mutex
	inflight   map[string]bool
	svcLocks   map[string]*sync.Mutex
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

func serve(envPath string) {
	env, err := config.LoadEnv(envPath)
	if err != nil {
		fatal(err)
	}
	reload := func() *config.Config {
		cfg, err := config.Load(env.Values, env.BackupConfigPath)
		if err != nil {
			fatal(err)
		}
		return cfg
	}

	h := http.NewServeMux()

	h.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		cfg := reload()
		rows := buildRows(env, cfg)
		render(w, "index.html", map[string]any{
			"Rows":  rows,
			"Port":  env.Port,
			"EnvPath": env.EnvPath,
			"OffsiteHost": offsiteHost(env, cfg),
		})
	})

	// Per-service logos. Resolution order:
	//  1. backup-services.yaml `logo:` or the service's app.yaml `icon:` (Homeio's field),
	//     either a direct URL (cached in the state dir) or a local path
	//  2. built-in embedded assets
	h.HandleFunc("GET /logo/{svc}", func(w http.ResponseWriter, r *http.Request) {
		svcName := r.PathValue("svc")
		src := ""
		if cfg := reload(); cfg != nil {
			if svc, ok := cfg.Services[svcName]; ok {
				src = svc.ResolveIcon(env, svcName)
			}
		}
		switch {
		case src == "":
		case strings.HasPrefix(src, "http://"), strings.HasPrefix(src, "https://"):
			if b, err := fetchLogo(env.StateDir, svcName, src); err == nil {
				w.Header().Set("Content-Type", sniffMime(b))
				w.Write(b)
				return
			}
		default:
			if b, err := os.ReadFile(src); err == nil {
				w.Header().Set("Content-Type", mimeOf(src))
				w.Write(b)
				return
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
		cfg := reload()
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

	addr := env.Addr + ":" + env.Port
	if env.Addr == "" {
		addr = "127.0.0.1:" + env.Port
	}
	srv := &http.Server{Addr: addr, Handler: logHandler(h), ReadHeaderTimeout: 10 * time.Second}
	srv.ListenAndServe()
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
		svcEnv := svc.ServiceEnv(env, svcName)
		_ = svcEnv
		for _, proc := range svc.Procedures {
			r := row{Service: svcName, Procedure: proc.ID, Type: proc.Type, Schedule: proc.Schedule}
			r.Logo = "/logo/" + svcName
			r.HasLogo = web.HasAsset("assets/"+svcName+".svg") || web.HasAsset("assets/"+svcName+".png") || svc.Logo != ""
			if c, err := schedule.Parse(proc.Schedule); err == nil {
				r.NextRun = c.Next(time.Now()).Format(time.RFC1123 )
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
	if host := cfg.Offsite.Host; host != "" {
		return host
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
		http.Error(w, "template error: "+err.Error(), 500) // format %v
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
	funcs := template.FuncMap{"fmtBytes": fmtBytes, "upper": strings.ToUpper}
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

// fetchLogo downloads and caches a remote icon in <state>/logos/<svc>.
// Cache lives for 24h; always returns the cached bytes on download failure
// (stale is better than nothing).
func fetchLogo(stateDir, svcName, url string) ([]byte, error) {
	dir := filepath.Join(stateDir, "logos")
	path := filepath.Join(dir, svcName)
	meta := path + ".meta"

	lookup := func() ([]byte, bool) {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 {
			return nil, false
		}
		if mi, err := os.Stat(meta); err == nil && time.Since(mi.ModTime()) > 24*time.Hour {
			return nil, false // cached but too old; try refresh below
		}
		b, err := os.ReadFile(path)
		return b, err == nil
	}
	if b, ok := lookup(); ok {
		return b, nil
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return os.ReadFile(path) // serve stale
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return os.ReadFile(path) // serve stale
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(path, b, 0o644)
	_ = os.Chtimes(meta, time.Now(), time.Now()) // only updates mtime if exists
	if _, err := os.Stat(meta); os.IsNotExist(err) {
		_ = os.WriteFile(meta, []byte(url), 0o644)
		return b, nil
	}
	return b, nil
}

// sniffMime picks a conservative content type for icon bytes.
func sniffMime(b []byte) string {
	if len(b) > 4 {
		if string(b[:4]) == "<svg" || string(b[:5]) == "<?xml" {
			return "image/svg+xml"
		}
		if b[0] == 0x89 && b[1] == 'P' {
			return "image/png"
		}
	}
	return mimeOf(".png")
}

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
