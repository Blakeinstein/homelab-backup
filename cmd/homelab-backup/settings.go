// Settings pages: root-level (.env) settings, restic/retention defaults,
// rsync/offsite targets, and the backups panel that edits the configured
// services & procedures directly in the backup-services.yaml file.
//
// All writes go to the real files (atomically, validate-then-rename) so the
// dashboard edits stay the single source of truth on disk.
package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"homelab-backup/internal/config"
	"homelab-backup/internal/schedule"
	"homelab-backup/internal/store"
	"homelab-backup/internal/yamledit"
	"gopkg.in/yaml.v3"
)

// ---- flash helpers ---------------------------------------------------------

const flashCookie = "hb_flash"

func flashSet(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(msg), Path: "/", MaxAge: 60})
}

// flashRead returns (and clears) the one-shot flash message for this request.
func flashRead(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	return v
}

func badFlash(w http.ResponseWriter, r *http.Request, target, msg string) {
	flashSet(w, "ERROR: "+msg)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func formStr(form url.Values, key string) string {
	return strings.TrimSpace(form.Get(key))
}

// ---- root settings page ----------------------------------------------------

type envField struct {
	Key, Hint, Value string
}

// envFields are the keys editable from the settings page (written back to
// the agent .env file, in this order).
var envFields = []envField{
	{"HOMELAB_BACKUP_CONFIG", "Path to backup-services.yaml. Pointing elsewhere re-targets the agent — the file must exist.", ""},
	{"HOMELAB_DATA_ROOT", "Target of file backup procedures.", ""},
	{"HOMELAB_BACKUP_STATE", "Restic password, run history and sched markers live here.", ""},
	{"HOMELAB_CONTAINER_BASE_DIR", "Base dir of service .env files (quadlet / credentials).", ""},
	{"RESTIC_REPOSITORY", "Fallback only — defaults.restic_repo in the yaml wins.", ""},
	{"HOMELAB_BACKUP_PORT", "Dashboard port; takes effect after a service restart.", ""},
	{"HOMELAB_BACKUP_ADDR", "Dashboard bind address (empty = 127.0.0.1); needs a restart.", ""},
	{"HOMELAB_BACKUP_LOG", "Optional log file path.", ""},
	{"OFFSITE_HOST", "Rsync host used when the yaml has no offsite.host.", ""},
	{"OFFSITE_PATH", "Rsync path used when the yaml has no offsite.path.", ""},
	{"OFFSITE_SSH_KEY", "rsync -e \"ssh -i <key>\" fallback when neither the yaml nor a target sets ssh_key.", ""},
	{"OFFSITE_MIN_HOURS", "Scheduled offsite pushes are gated to at most one per N hours.", ""},
}

func settingsRootHandler(env *config.Env) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"Nav":        navData{Current: "/settings"},
			"Port":       env.Port,
			"EnvPath":    env.EnvPath,
			"ConfigPath": env.BackupConfigPath,
			"Flash":      flashRead(w, r),
			"Fields":     envFieldsWith(env),
		}
		cfgRaw, rawErr := rawConfig(env)
		if rawErr != nil {
			data["CfgError"] = rawErr.Error()
		} else {
			data["Defaults"] = map[string]any{
				"ResticRepo": cfgRaw.Defaults.ResticRepo,
				"KeepDaily":  cfgRaw.Defaults.Retention.KeepDaily,
				"KeepWeekly": cfgRaw.Defaults.Retention.KeepWeekly,
			}
			data["Offsite"] = offsiteView(env, cfgRaw)
			data["Recent"] = recentOffsite(env)
		}
		render(w, "settings.html", data)
	}
}

func envFieldsWith(env *config.Env) []envField {
	out := make([]envField, len(envFields))
	copy(out, envFields)
	for i := range out {
		out[i].Value = env.Values[out[i].Key]
	}
	return out
}

// rawConfig parses the yaml WITHOUT ${VAR} expansion and without dropping
// disabled services — the view used by the settings pages.
func rawConfig(env *config.Env) (*config.Config, error) {
	data, err := os.ReadFile(env.BackupConfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", env.BackupConfigPath, err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", env.BackupConfigPath, err)
	}
	return &cfg, nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- visible data for the offsite/rsync card -------------------------------

type targetRow struct {
	Name, Host, Path, SSHUser, SSHKey, Flags string
	Enabled                                  bool
}

type offsiteData struct {
	Enabled bool
	SSHUser string
	SSHKey  string
	Host    string
	Path    string
	Flags   string
	Targets []targetRow
}

func offsiteView(env *config.Env, cfgRaw *config.Config) offsiteData {
	o := offsiteData{
		Enabled: cfgRaw.Offsite.Enabled,
		SSHUser: cfgRaw.Offsite.SSHUser,
		SSHKey:  cfgRaw.Offsite.SSHKey,
		Host:    cfgRaw.Offsite.Host,
		Path:    cfgRaw.Offsite.Path,
		Flags:   cfgRaw.Offsite.Flags,
	}
	if o.Host == "" {
		o.Host = env.OffsiteHost
	}
	if o.Path == "" {
		o.Path = env.OffsitePath
	}
	for _, t := range cfgRaw.Offsite.Targets {
		o.Targets = append(o.Targets, targetRow{
			Name: t.Name, Host: t.Host, Path: t.Path,
			SSHUser: t.SSHUser, SSHKey: t.SSHKey, Flags: t.Flags,
			Enabled: t.IsEnabled(),
		})
	}
	return o
}

func recentOffsite(env *config.Env) []store.Run {
	all, _, err := store.Load(env.StateDir, 200)
	if err != nil {
		return nil
	}
	var out []store.Run
	for _, r := range all {
		if r.Service == "offsite" {
			out = append(out, r)
			if len(out) >= 8 {
				break
			}
		}
	}
	return out
}

// ---- yaml write helper (validate-then-atomic-replace) -----------------------

func saveValidatedYAML(env *config.Env, doc *yaml.Node, path string) error {
	b, err := yamledit.Marshal(doc)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()
	if _, err := config.Load(env.Values, tmpPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("resulting yaml invalid: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// afterYAMLChange reports the secondary message of a timers regeneration;
// failures are advisory and surfaced through the flash message.
func afterYAMLChange(env *config.Env) string {
	cfg, err := config.Load(env.Values, env.BackupConfigPath)
	if err != nil {
		return "yaml invalid after save: " + err.Error()
	}
	if err := installTimers(env, cfg); err != nil {
		return "saved, but timers reinstall failed: " + err.Error()
	}
	return "saved; systemd timers re-installed"
}

// loadRootYAML loads the config document and its top-level mapping.
func loadRootYAML(env *config.Env) (*yaml.Node, *yaml.Node, error) {
	doc, err := yamledit.Load(env.BackupConfigPath)
	if err != nil {
		return nil, nil, err
	}
	root, err := yamledit.M(doc)
	if err != nil {
		return nil, nil, err
	}
	return doc, root, nil
}

// ---- POST: root settings (.env file) ---------------------------------------

func settingsEnvSave(w http.ResponseWriter, r *http.Request, env *config.Env) {
	if err := r.ParseForm(); err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	updates := map[string]string{}
	for _, f := range envFields {
		if !r.Form.Has(f.Key) {
			continue // not posted → unchanged (partial posts allowed)
		}
		val := formStr(r.Form, f.Key)
		if val != env.Values[f.Key] {
			updates[f.Key] = val
		}
	}
	for _, k := range []string{"HOMELAB_BACKUP_CONFIG", "HOMELAB_DATA_ROOT", "HOMELAB_BACKUP_STATE", "HOMELAB_CONTAINER_BASE_DIR"} {
		if v, ok := updates[k]; ok && v == "" {
			badFlash(w, r, "/settings", k+" cannot be empty (nothing changed)")
			return
		}
	}
	if p, ok := updates["HOMELAB_BACKUP_CONFIG"]; ok {
		if _, err := os.Stat(p); err != nil {
			badFlash(w, r, "/settings", fmt.Sprintf("backups config %s: %v (nothing changed)", p, err))
			return
		}
	}
	if v, ok := updates["OFFSITE_MIN_HOURS"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			badFlash(w, r, "/settings", "OFFSITE_MIN_HOURS must be a positive integer (nothing changed)")
			return
		}
	}
	if len(updates) == 0 {
		flashSet(w, "No changes to save")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if err := writeEnvFile(env, updates); err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	flashSet(w, fmt.Sprintf("Saved %d setting(s) to %s (runtime paths apply immediately; port/bind need a service restart)", len(updates), env.EnvPath))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// writeEnvFile rewrites KEY=... lines in place (comments preserved) and
// appends missing keys at the end, atomically. The running env is updated.
func writeEnvFile(env *config.Env, updates map[string]string) error {
	var lines []string
	if data, err := os.ReadFile(env.EnvPath); err == nil {
		lines = splitEnvLines(string(data))
	}
	pending := map[string]bool{}
	for k := range updates {
		pending[k] = true
	}
	for i, line := range lines {
		prefix := "export "
		if !strings.HasPrefix(line, prefix) {
			prefix = ""
		}
		bare := strings.TrimPrefix(line, "export ")
		key, _, ok := strings.Cut(bare, "=")
		if !ok || strings.ContainsAny(key, " \t") {
			continue
		}
		key = strings.TrimSpace(key)
		if pending[key] {
			lines[i] = prefix + key + "=" + updates[key]
			pending[key] = false
		}
	}
	for _, f := range envFields {
		if pending[f.Key] {
			lines = append(lines, f.Key+"="+updates[f.Key])
		}
	}
	out := ""
	if len(lines) > 0 {
		out = strings.Join(lines, "\n") + "\n"
	}
	dir := filepath.Dir(env.EnvPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(env.EnvPath)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, env.EnvPath); err != nil {
		os.Remove(name)
		return err
	}
	// apply to the running process
	for k, v := range updates {
		env.Values[k] = v
	}
	env.BackupConfigPath = env.Values["HOMELAB_BACKUP_CONFIG"]
	env.DataRoot = env.Values["HOMELAB_DATA_ROOT"]
	env.StateDir = env.Values["HOMELAB_BACKUP_STATE"]
	env.ContainerBaseDir = env.Values["HOMELAB_CONTAINER_BASE_DIR"]
	env.Port = envOrDefault(env.Values, "HOMELAB_BACKUP_PORT", "3095")
	env.Addr = env.Values["HOMELAB_BACKUP_ADDR"]
	env.OffsiteHost = env.Values["OFFSITE_HOST"]
	env.OffsitePath = env.Values["OFFSITE_PATH"]
	return nil
}

func splitEnvLines(data string) []string {
	s := strings.TrimSuffix(data, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ---- POST: defaults (restic repo + retention) ------------------------------

func settingsDefaultsSave(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	doc, root, err := loadRootYAML(env)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	defs, err := yamledit.Map(root, "defaults", true)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	repo := formStr(r.Form, "restic_repo")
	if repo == "" {
		badFlash(w, r, "/settings", "restic repository path is required (nothing changed)")
		return
	}
	yamledit.SetString(defs, "restic_repo", repo)
	ret, err := yamledit.Map(defs, "retention", true)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	// clean up stray keys from earlier flat writes (kept for compat)
	yamledit.Remove(defs, "keep_daily")
	yamledit.Remove(defs, "keep_weekly")
	for _, key := range []string{"keep_daily", "keep_weekly"} {
		v := formStr(r.Form, key)
		if v == "" || v == "0" {
			yamledit.Remove(ret, key)
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badFlash(w, r, "/settings", "retention "+key+" must be a non-negative number (nothing changed)")
			return
		}
		yamledit.SetInt(ret, key, n)
	}
	if err := saveValidatedYAML(env, doc, env.BackupConfigPath); err != nil {
		badFlash(w, r, "/settings", "yaml: "+err.Error())
		return
	}
	msg := afterYAMLChange(env)
	flashSet(w, "Saved backup defaults — " + msg)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// ---- POST: offsite master + legacy primary ---------------------------------

func settingsOffsiteSave(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	doc, root, err := loadRootYAML(env)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	off, err := yamledit.Map(root, "offsite", true)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	yamledit.SetBool(off, "enabled", r.Form.Has("enabled"))
	for _, key := range []string{"ssh_user", "ssh_key", "flags"} {
		setOrRemove(off, key, formStr(r.Form, key))
	}
	for _, key := range []string{"host", "path"} {
		v := formStr(r.Form, key)
		if v == "" {
			yamledit.Remove(off, key) // fall back to the .env values
			continue
		}
		yamledit.SetString(off, key, v)
	}
	if err := saveValidatedYAML(env, doc, env.BackupConfigPath); err != nil {
		badFlash(w, r, "/settings", "yaml: "+err.Error())
		return
	}
	flashSet(w, "Saved offsite settings (host/path apply only while no rsync targets are listed)")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// ---- POST: rsync targets ----------------------------------------------------

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func settingsTargetSave(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	name := formStr(r.Form, "name")
	host := formStr(r.Form, "host")
	path := formStr(r.Form, "path")
	if name == "" || !nameRe.MatchString(name) {
		badFlash(w, r, "/settings", "target name required (letters, digits, . _ -)")
		return
	}
	if name == "primary" {
		badFlash(w, r, "/settings", "target name 'primary' is reserved")
		return
	}
	if host == "" || path == "" {
		badFlash(w, r, "/settings", "target host and path are required")
		return
	}
	doc, root, err := loadRootYAML(env)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	off, err := yamledit.Map(root, "offsite", true)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	targets, err := yamledit.Seq(off, "targets", true)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	t := findTarget(targets, name)
	if t == nil {
		t = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		yamledit.SetString(t, "name", name)
		targets.Content = append(targets.Content, t)
	}
	yamledit.SetString(t, "host", host)
	yamledit.SetString(t, "path", path)
	setOrRemove(t, "ssh_user", formStr(r.Form, "ssh_user"))
	setOrRemove(t, "ssh_key", formStr(r.Form, "ssh_key"))
	setOrRemove(t, "flags", formStr(r.Form, "flags"))
	if r.Form.Has("enabled") {
		yamledit.Remove(t, "enabled") // absence = enabled (default)
	} else {
		yamledit.SetBool(t, "enabled", false)
	}
	if err := saveValidatedYAML(env, doc, env.BackupConfigPath); err != nil {
		badFlash(w, r, "/settings", "yaml: "+err.Error())
		return
	}
	flashSet(w, fmt.Sprintf("Saved rsync target %s. Offsite push now uses the target list only (host/path above are ignored).", name))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func settingsTargetDelete(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	name := formStr(r.Form, "name")
	doc, root, err := loadRootYAML(env)
	if err != nil {
		badFlash(w, r, "/settings", err.Error())
		return
	}
	off, err := yamledit.Map(root, "offsite", false)
	if err != nil {
		badFlash(w, r, "/settings", "no offsite section in yaml")
		return
	}
	targets, err := yamledit.Seq(off, "targets", false)
	if err != nil {
		badFlash(w, r, "/settings", "no rsync targets configured")
		return
	}
	idx := -1
	for i, el := range targets.Content {
		if el.Kind == yaml.MappingNode && yamledit.Get(el, "name") == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		badFlash(w, r, "/settings", "no rsync target named "+name)
		return
	}
	targets.Content = append(targets.Content[:idx], targets.Content[idx+1:]...)
	if err := saveValidatedYAML(env, doc, env.BackupConfigPath); err != nil {
		badFlash(w, r, "/settings", "yaml: "+err.Error())
		return
	}
	flashSet(w, "Deleted rsync target " + name)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// setOrRemove writes key when val is non-empty, deletes the key otherwise.
func setOrRemove(m *yaml.Node, key, val string) {
	if val == "" {
		yamledit.Remove(m, key)
		return
	}
	yamledit.SetString(m, key, val)
}

// findTarget locates a target mapping node by name inside a targets seq.
func findTarget(targets *yaml.Node, name string) *yaml.Node {
	for _, el := range targets.Content {
		if el.Kind == yaml.MappingNode && yamledit.Get(el, "name") == name {
			return el
		}
	}
	return nil
}

// minimalCfg builds a bare config when the yaml can't load, so restic-backed
// pages (snapshots/downloads) still work off the env's restic repo.
func minimalCfg(env *config.Env) *config.Config {
	return &config.Config{
		Defaults: config.Defaults{ResticRepo: envOrDefault(env.Values, "RESTIC_REPOSITORY", "")},
		Offsite:  config.OffsiteYAML{Host: env.OffsiteHost, Path: env.OffsitePath},
	}
}

func envOrDefault(vals map[string]string, key, def string) string {
	if v := vals[key]; v != "" {
		return v
	}
	return def
}

// ---- backups panel (edit services & procedures in the yaml) -----------------

type procRow struct {
	Svc         string
	ID          string
	Type        string
	Schedule    string
	Container   string
	DBUser      string
	DBPassEnv   string
	Paths       string // newline-separated for the textarea
}

type svcRow struct {
	Name     string
	Disabled bool
	EnvFile  string
	Logo     string
	Procs    []procRow
}

func backupsHandler(env *config.Env) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var services []svcRow
		cfgRaw, rawErr := rawConfig(env)
		if rawErr == nil {
			names := make([]string, 0, len(cfgRaw.Services))
			for n := range cfgRaw.Services {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				s := cfgRaw.Services[n]
				row := svcRow{
					Name: n, Disabled: s.Disabled,
					EnvFile: s.EnvFile, Logo: s.Logo,
				}
				for _, p := range s.Procedures {
					row.Procs = append(row.Procs, procRow{
						Svc: n, ID: p.ID, Type: p.Type, Schedule: p.Schedule,
						Container: p.Container, DBUser: p.DBUser, DBPassEnv: p.DBPassEnv,
						Paths: strings.Join(p.Paths, "\n"),
					})
				}
				services = append(services, row)
			}
		}
		render(w, "settings_backups.html", map[string]any{
			"Nav":        navData{Current: "/settings/backups"},
			"Port":       env.Port,
			"EnvPath":    env.EnvPath,
			"ConfigPath": env.BackupConfigPath,
			"Flash":      flashRead(w, r),
			"Services":   services,
			"CfgError":   errText(rawErr),
		})
	}
}

// validateProcForm checks the form values for one procedure.
func validateProcForm(typ, schedRaw, container, dbUser string, paths []string) string {
	if schedRaw == "" {
		return "schedule is required"
	}
	if _, err := schedule.Parse(schedRaw); err != nil {
		return "bad schedule: " + err.Error()
	}
	switch typ {
	case "files":
		if len(paths) == 0 {
			return "files procedures need at least one path"
		}
	case "postgres_dump":
		if container == "" || dbUser == "" {
			return "postgres_dump procedures need container and db_user"
		}
	case "mariadb_dump":
		if container == "" || dbUser == "" {
			return "mariadb_dump procedures need container and db_user"
		}
	default:
		return "type must be files, postgres_dump or mariadb_dump"
	}
	return ""
}

func pathsFromForm(r *http.Request) []string {
	var out []string
	for _, line := range strings.Split(r.Form.Get("paths"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// upsertProc writes one procedure into a service's Procedures list of the
// yaml (in place when the original id matches, appended for new ones).
func upsertProc(env *config.Env, svcName, origID string, r *http.Request) (string, error) {
	procID := formStr(r.Form, "id")
	typ := formStr(r.Form, "type")
	schedRaw := strings.Join(strings.Fields(r.Form.Get("schedule")), " ")
	container := formStr(r.Form, "container")
	dbUser := formStr(r.Form, "db_user")
	dbPassEnv := formStr(r.Form, "db_password_env")
	paths := pathsFromForm(r)
	if procID == "" || !nameRe.MatchString(procID) {
		return "", fmt.Errorf("procedure id required (letters, digits, - _)")
	}
	if msg := validateProcForm(typ, schedRaw, container, dbUser, paths); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	doc, root, err := loadRootYAML(env)
	if err != nil {
		return "", err
	}
	services, err := yamledit.Map(root, "services", true)
	if err != nil {
		return "", err
	}
	svc, err := yamledit.Map(services, svcName, true)
	if err != nil {
		return "", err
	}
	procs, err := yamledit.Seq(svc, "procedures", true)
	if err != nil {
		return "", err
	}
	p := findProc(procs, origID)
	if p == nil {
		if origID != "" {
			return "", fmt.Errorf("procedure %q not found in service %s (was it renamed already?)", origID, svcName)
		}
		if findProc(procs, procID) != nil {
			return "", fmt.Errorf("procedure %q already exists in service %s", procID, svcName)
		}
		p = appendProc(procs)
	}
	writeProc(p, procID, typ, schedRaw, container, dbUser, dbPassEnv, paths)
	if err := saveValidatedYAML(env, doc, env.BackupConfigPath); err != nil {
		return "", err
	}
	return procID, nil
}

func writeProc(p *yaml.Node, id, typ, sched, container, dbUser, dbPassEnv string, paths []string) {
	yamledit.SetString(p, "id", id)
	yamledit.SetString(p, "type", typ)
	yamledit.SetString(p, "schedule", sched)
	if typ == "files" {
		yamledit.SetStringSeq(p, "paths", paths)
		yamledit.Remove(p, "container")
		yamledit.Remove(p, "db_user")
		yamledit.Remove(p, "db_password_env")
	} else {
		yamledit.Remove(p, "paths")
		yamledit.SetString(p, "container", container)
		yamledit.SetString(p, "db_user", dbUser)
		if dbPassEnv == "" {
			yamledit.Remove(p, "db_password_env")
		} else {
			yamledit.SetString(p, "db_password_env", dbPassEnv)
		}
	}
}

func findProc(procs *yaml.Node, id string) *yaml.Node {
	for _, el := range procs.Content {
		if el.Kind == yaml.MappingNode && yamledit.Get(el, "id") == id {
			return el
		}
	}
	return nil
}

// appendProc adds a new procedure mapping to a procedures sequence.
func appendProc(procs *yaml.Node) *yaml.Node {
	p := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	procs.Content = append(procs.Content, p)
	return p
}

// procDelete removes one procedure mapping from a service.
func procDelete(env *config.Env, svcName, procID string) error {
	doc, root, err := loadRootYAML(env)
	if err != nil {
		return err
	}
	services, err := yamledit.Map(root, "services", false)
	if err != nil {
		return fmt.Errorf("no services in yaml")
	}
	svc, err := yamledit.Map(services, svcName, false)
	if err != nil {
		return fmt.Errorf("service %s not found", svcName)
	}
	procs, err := yamledit.Seq(svc, "procedures", false)
	if err != nil {
		return fmt.Errorf("service %s has no procedures", svcName)
	}
	idx := -1
	for i, el := range procs.Content {
		if el.Kind == yaml.MappingNode && yamledit.Get(el, "id") == procID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("procedure %s not found in service %s", procID, svcName)
	}
	procs.Content = append(procs.Content[:idx], procs.Content[idx+1:]...)
	return saveValidatedYAML(env, doc, env.BackupConfigPath)
}

// svcUpsert adds or updates a service block (env_file, logo, disabled flag).
// origName == "" adds; a non-empty new name renames the block in place.
func svcUpsert(env *config.Env, origName, name string, r *http.Request) error {
	if name == "" || !nameRe.MatchString(name) {
		return fmt.Errorf("service name required (letters, digits, - _)")
	}
	if origName == "" {
		if err := svcAdd(env, name, r); err != nil {
			return err
		}
	}
	return svcUpsertFields(env, origName, name, r)
}

// svcAdd creates a service block with one starter files procedure.
func svcAdd(env *config.Env, name string, r *http.Request) error {
	doc, root, err := loadRootYAML(env)
	if err != nil {
		return err
	}
	services, err := yamledit.Map(root, "services", true)
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		if services.Content[i].Value == name {
			return fmt.Errorf("service %q already exists", name)
		}
	}
	svc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setOrRemove(svc, "env_file", formStr(r.Form, "env_file"))
	procs := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	yamledit.SetString(n, "id", name+"-data")
	yamledit.SetString(n, "type", "files")
	yamledit.SetString(n, "schedule", "0 5 * * *")
	yamledit.SetStringSeq(n, "paths", []string{env.DataRoot + "/" + name})
	procs.Content = append(procs.Content, n)
	yamledit.SetValue(svc, "procedures", procs)
	services.Content = append(services.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, svc)
	return saveValidatedYAML(env, doc, env.BackupConfigPath)
}

// svcUpsertFields writes the editable service fields (env_file/logo/disabled).
func svcUpsertFields(env *config.Env, origName, name string, r *http.Request) error {
	if origName != "" && origName != name {
		return renameService(env, origName, name)
	}
	doc, root, err := loadRootYAML(env)
	if err != nil {
		return err
	}
	services, err := yamledit.Map(root, "services", false)
	if err != nil {
		return fmt.Errorf("no services in yaml")
	}
	svc, err := yamledit.Map(services, name, false)
	if err != nil {
		return err
	}
	setOrRemove(svc, "env_file", formStr(r.Form, "env_file"))
	setOrRemove(svc, "logo", formStr(r.Form, "logo"))
	if r.Form.Has("disabled") {
		yamledit.SetBool(svc, "disabled", true)
	} else {
		yamledit.Remove(svc, "disabled")
	}
	if err := saveValidatedYAML(env, doc, env.BackupConfigPath); err != nil {
		return err
	}
	return nil
}

// mapKeyPair renames a mapping key in place, keeping key order (and morphs
// the value node in place so comments survive).
func renameService(env *config.Env, origName, name string) error {
	doc, root, err := loadRootYAML(env)
	if err != nil {
		return err
	}
	services, err := yamledit.Map(root, "services", false)
	if err != nil {
		return fmt.Errorf("no services in yaml")
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		if services.Content[i].Value == origName {
			services.Content[i].Value = name
			break
		}
	}
	return saveValidatedYAML(env, doc, env.BackupConfigPath)
}

func serviceDelete(env *config.Env, name string) error {
	doc, root, err := loadRootYAML(env)
	if err != nil {
		return err
	}
	services, err := yamledit.Map(root, "services", false)
	if err != nil {
		return fmt.Errorf("no services in yaml")
	}
	yamledit.Remove(services, name)
	return saveValidatedYAML(env, doc, env.BackupConfigPath)
}

// ---- POST wrappers (parse form → apply → flash → redirect) ------------------

func backupsServiceSave(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	origName := formStr(r.Form, "orig")
	name := formStr(r.Form, "name")
	if err := svcUpsert(env, origName, name, r); err != nil {
		badFlash(w, r, "/settings/backups", err.Error())
		return
	}
	msg := afterYAMLChange(env)
	verb := "Updated"
	if origName == "" {
		verb = "Added"
	}
	flashSet(w, fmt.Sprintf("%s service %s — %s", verb, name, msg))
	http.Redirect(w, r, "/settings/backups", http.StatusSeeOther)
}

func backupsServiceDelete(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	name := formStr(r.Form, "name")
	if err := serviceDelete(env, name); err != nil {
		badFlash(w, r, "/settings/backups", err.Error())
		return
	}
	msg := afterYAMLChange(env)
	flashSet(w, fmt.Sprintf("Deleted service %s — %s", name, msg))
	http.Redirect(w, r, "/settings/backups", http.StatusSeeOther)
}

func backupsProcSave(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	svcName := formStr(r.Form, "svc")
	origID := formStr(r.Form, "orig")
	id, err := upsertProc(env, svcName, origID, r)
	if err != nil {
		badFlash(w, r, "/settings/backups", err.Error())
		return
	}
	msg := afterYAMLChange(env)
	verb := "Updated"
	if origID == "" {
		verb = "Added"
	}
	flashSet(w, fmt.Sprintf("%s procedure %s/%s — %s", verb, svcName, id, msg))
	http.Redirect(w, r, "/settings/backups", http.StatusSeeOther)
}

func backupsProcDelete(w http.ResponseWriter, r *http.Request, env *config.Env) {
	r.ParseForm()
	svcName := formStr(r.Form, "svc")
	procID := formStr(r.Form, "id")
	if err := procDelete(env, svcName, procID); err != nil {
		badFlash(w, r, "/settings/backups", err.Error())
		return
	}
	msg := afterYAMLChange(env)
	flashSet(w, fmt.Sprintf("Deleted procedure %s/%s — %s", svcName, procID, msg))
	http.Redirect(w, r, "/settings/backups", http.StatusSeeOther)
}

func backupsTimers(w http.ResponseWriter, r *http.Request, env *config.Env) {
	cfg, err := config.Load(env.Values, env.BackupConfigPath)
	if err != nil {
		badFlash(w, r, "/settings/backups", err.Error())
		return
	}
	if err := installTimers(env, cfg); err != nil {
		badFlash(w, r, "/settings/backups", "timers reinstall failed: "+err.Error())
		return
	}
	flashSet(w, "Systemd user timers re-installed from the current yaml")
	http.Redirect(w, r, "/settings/backups", http.StatusSeeOther)
}
