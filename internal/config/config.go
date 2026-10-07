// Package config loads the service's own .env file and the
// backup-services.yaml (single source of truth) that it points to,
// expanding ${VAR} references against the merged environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvFile is the resolved set of variables from the agent's .env file.
type Env struct {
	Values           map[string]string
	BackupConfigPath string // HOMELAB_BACKUP_CONFIG
	DataRoot         string
	StateDir         string
	ContainerBaseDir string
	Port             string
	Addr             string
	OffsiteHost      string
	OffsitePath      string
	EnvPath          string
}

// Config is the parsed backup-services.yaml.
type Config struct {
	Defaults Defaults            `yaml:"defaults"`
	Services map[string]*Service `yaml:"services"`
	Offsite  OffsiteYAML         `yaml:"offsite"`
}

type Defaults struct {
	ResticRepo string    `yaml:"restic_repo"`
	Retention  Retention `yaml:"retention"`
}

type Retention struct {
	KeepDaily  int `yaml:"keep_daily"`
	KeepWeekly int `yaml:"keep_weekly"`
}

type OffsiteYAML struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
	Path    string `yaml:"path"`
	Flags   string `yaml:"flags"`
	SSHUser string `yaml:"ssh_user"`
	SSHKey  string `yaml:"ssh_key"`
	// SSHPassword enables password-based ssh auth via sshpass. Prefer a
	// key, but some rental boxes only ship a root password: set it here or
	// in OFFSITE_SSH_PASS in the .env (keeps it out of the yaml and logs).
	SSHPassword string `yaml:"ssh_password,omitempty"`

	// Targets is the optional list of rsync destinations. When present it
	// replaces the legacy single Host/Path target above.
	Targets []OffsiteTarget `yaml:"targets,omitempty"`
}

// OffsiteTarget is one offsite push destination. Set either host+path
// (rsync over ssh) or remote (any rclone remote path, e.g. a gdrive or
// crypt remote); when remote is set the rsync fields are ignored.
type OffsiteTarget struct {
	Name    string `yaml:"name"`
	Enabled *bool  `yaml:"enabled,omitempty"` // absent = enabled
	Host    string `yaml:"host,omitempty"`
	Path    string `yaml:"path,omitempty"`
	SSHUser string `yaml:"ssh_user,omitempty"`
	SSHKey  string `yaml:"ssh_key,omitempty"`
	// password auth via sshpass (see OffsiteYAML note); overrides nothing —
	// ssh_key wins if both are set.
	SSHPassword string `yaml:"ssh_password,omitempty"`
	Remote      string `yaml:"remote,omitempty"`
	Flags       string `yaml:"flags,omitempty"`
}

// IsEnabled reports whether the target should participate in pushes.
func (t *OffsiteTarget) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// Destination renders the rsync [user@]host:path form.
func (t *OffsiteTarget) Destination() string {
	user := ""
	if t.SSHUser != "" {
		user = t.SSHUser + "@"
	}
	return user + t.Host + ":" + t.Path
}

// OffsiteTargets returns the effective push destinations: the explicit
// targets list when present, otherwise the legacy single Host/Path (with
// .env fallbacks for host/path/ssh key).
func (c *Config) OffsiteTargets(env *Env) []OffsiteTarget {
	var out []OffsiteTarget
	keyFallback, passFallback := "", ""
	if env != nil {
		keyFallback = env.Values["OFFSITE_SSH_KEY"]
		passFallback = env.Values["OFFSITE_SSH_PASS"]
	}
	if len(c.Offsite.Targets) > 0 {
		for _, t := range c.Offsite.Targets {
			if !t.IsEnabled() {
				continue
			}
			if t.SSHKey == "" {
				t.SSHKey = keyFallback
			}
			if t.SSHPassword == "" {
				t.SSHPassword = passFallback
			}
			out = append(out, t)
		}
		return out
	}
	host, path := c.Offsite.Host, c.Offsite.Path
	if host == "" && env != nil {
		host = env.OffsiteHost
	}
	if path == "" && env != nil {
		path = env.OffsitePath
	}
	if host == "" || path == "" {
		return nil
	}
	key := c.Offsite.SSHKey
	if key == "" {
		key = keyFallback
	}
	pass := c.Offsite.SSHPassword
	if pass == "" {
		pass = passFallback
	}
	return append(out, OffsiteTarget{
		Name: "primary", Enabled: bPtr(true),
		Host: host, Path: path,
		SSHUser: c.Offsite.SSHUser, SSHKey: key, SSHPassword: pass, Flags: c.Offsite.Flags,
	})
}

func bPtr(v bool) *bool { return &v }

type Service struct {
	Disabled   bool         `yaml:"disabled"`
	EnvFile    string       `yaml:"env_file"`
	Logo       string       `yaml:"logo,omitempty"` // optional icon override: URL or local path
	Procedures []*Procedure `yaml:"procedures"`

	// envVars holds the per-service credentials, loaded from EnvFile at runtime.
	envVars map[string]string
}

type Procedure struct {
	ID        string   `yaml:"id"`
	Type      string   `yaml:"type"` // postgres_dump | mariadb_dump | files
	Schedule  string   `yaml:"schedule"`
	Container string   `yaml:"container,omitempty"`
	DBUser    string   `yaml:"db_user,omitempty"`
	DBPassEnv string   `yaml:"db_password_env,omitempty"`
	Paths     []string `yaml:"paths,omitempty"`
	TarName   string   `yaml:"tar_name,omitempty"`
}

// LoadEnv parses the agent .env file. Precedence: real env > file.
// Defaults are merged from .env.example values baked into the binary.
func LoadEnv(path string) (*Env, error) {
	if path == "" {
		path = os.Getenv("HOMELAB_BACKUP_ENV")
	}
	if path == "" {
		// Look next to the binary, then in ~/.config/homelab-backup/
		path = filepath.Join(configDir(), "homelab-backup.env")
	}
	values := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading env file %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := values[strings.TrimSpace(k)]; !exists {
			values[strings.TrimSpace(k)] = v
		}
	}
	// Real environment wins.
	for _, k := range []string{
		"HOMELAB_BACKUP_CONFIG",
		"HOMELAB_DATA_ROOT",
		"HOMELAB_BACKUP_STATE",
		"HOMELAB_CONTAINER_BASE_DIR",
		"HOMELAB_BACKUP_PORT",
		"OFFSITE_HOST",
		"OFFSITE_PATH",
		"RESTIC_REPOSITORY",
		"HOMELAB_BACKUP_LOG",
	} {
		if v := os.Getenv(k); v != "" {
			values[k] = v
		} else if _, exists := values[k]; !exists {
			values[k] = ""
		}
	}
	for _, k := range []string{"HOMELAB_BACKUP_CONFIG", "HOMELAB_DATA_ROOT", "HOMELAB_BACKUP_STATE", "HOMELAB_CONTAINER_BASE_DIR"} {
		if values[k] == "" {
			return nil, fmt.Errorf("env file %s is missing required key %s", path, k)
		}
	}
	return &Env{
		Values:           values,
		BackupConfigPath: values["HOMELAB_BACKUP_CONFIG"],
		DataRoot:         values["HOMELAB_DATA_ROOT"],
		StateDir:         values["HOMELAB_BACKUP_STATE"],
		ContainerBaseDir: values["HOMELAB_CONTAINER_BASE_DIR"],
		Port:             valueOr(values, "HOMELAB_BACKUP_PORT", "3095"),
		Addr:             valueOr(values, "HOMELAB_BACKUP_ADDR", ""),
		OffsiteHost:      values["OFFSITE_HOST"],
		OffsitePath:      values["OFFSITE_PATH"],
		EnvPath:          path,
	}, nil
}

func valueOr(values map[string]string, key, def string) string {
	if v := values[key]; v != "" {
		return v
	}
	return def
}

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "homelab-backup")
}

// Load reads and validates the backup-services.yaml, expanding ${VAR}
// against the merged environment (agent env + process env).
func Load(envValues map[string]string, yamlPath string) (*Config, error) {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("reading backup config %s: %w", yamlPath, err)
	}
	expanded := expand(string(data), envValues)
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", yamlPath, err)
	}
	if cfg.Defaults.ResticRepo == "" {
		return nil, fmt.Errorf("%s: defaults.restic_repo is required", yamlPath)
	}
	for svcName, svc := range cfg.Services {
		if svc.Disabled {
			delete(cfg.Services, svcName)
			continue
		}
		if len(svc.Procedures) == 0 {
			return nil, fmt.Errorf("%s: service %s has no procedures", yamlPath, svcName)
		}
		for _, p := range svc.Procedures {
			if p.ID == "" {
				return nil, fmt.Errorf("%s: service %s has a procedure with no id", yamlPath, svcName)
			}
			if p.Schedule == "" {
				return nil, fmt.Errorf("%s: procedure %s/%s has no schedule", yamlPath, svcName, p.ID)
			}
		}
	}
	return &cfg, nil
}

// ResolveIcon returns the effective icon source for a service:
//  1. backup-services.yaml `logo:` (URL or local path)
//  2. the app.yaml `icon:` next to the service units (Homeio's field — same
//     value Homeio itself uses for the app tile icon)
//
// Empty when neither is set.
func (s *Service) ResolveIcon(env *Env, svcName string) string {
	if s.Logo != "" {
		return s.Logo
	}
	dir := ""
	if p := s.EnvFile; p != "" && !strings.Contains(p, "${") {
		dir = filepath.Dir(p)
	}
	if dir == "" {
		dir = filepath.Join(env.ContainerBaseDir, svcName)
	}
	appYaml := filepath.Join(dir, "app.yaml")
	data, err := os.ReadFile(appYaml)
	if err != nil {
		return ""
	}
	var app struct {
		Icon string `yaml:"icon"`
	}
	if yaml.Unmarshal(data, &app) == nil {
		return app.Icon
	}
	return ""
}

// LoadServiceEnv reads a service's own .env file for credentials.
// Falls back to <ContainerBaseDir>/<svc>/<svc>.env when the yaml uses ${HOMELAB_CONTAINER_BASE_DIR}
// but the expansion left a placeholder.
func (s *Service) LoadServiceEnv(env *Env, svcName string) map[string]string {
	if s.envVars == nil {
		s.envVars = map[string]string{}
	}
	path := s.EnvFile
	if path == "" || strings.Contains(path, "${") {
		path = filepath.Join(env.ContainerBaseDir, svcName, svcName+".env")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		s.envVars["__load_error__"] = err.Error()
		return s.envVars
	}
	s.envVars["__load_error__"] = ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "export ") {
			line = strings.TrimPrefix(line, "export ")
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		s.envVars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return s.envVars
}

// ServiceEnv returns the loaded per-service env (and loads it if needed).
func (s *Service) ServiceEnv(env *Env, svcName string) map[string]string {
	if s.envVars == nil {
		return s.LoadServiceEnv(env, svcName)
	}
	return s.envVars
}

// expand substitutes ${VAR} and bare $VAR occurrences using overrides
// (agent env) with process env fallback.
func expand(s string, overrides map[string]string) string {
	get := func(name string) string {
		if v := overrides[name]; v != "" {
			return v
		}
		return os.Getenv(name)
	}
	out := s
	// ${VAR} first (longest match, allows var names followed by '_')
	for {
		start := strings.Index(out, "${")
		if start < 0 {
			break
		}
		end := strings.Index(out[start:], "}")
		if end < 0 {
			break
		}
		end += start
		out = out[:start] + get(out[start+2:end]) + out[end+1:]
	}
	// then bare $VAR
	var b strings.Builder
	for i := 0; i < len(out); i++ {
		if out[i] == '$' && i+1 < len(out) && isNameChar(out[i+1]) {
			j := i + 1
			for j < len(out) && isNameChar(out[j]) {
				j++
			}
			b.WriteString(get(out[i+1 : j]))
			i = j - 1
			continue
		}
		b.WriteByte(out[i])
	}
	return b.String()
}

func isNameChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
