// Timer generation: converts the yaml schedules into systemd user
// timer/service units under ~/.config/systemd/user/.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"homelab-backup/internal/config"
	"homelab-backup/internal/schedule"
)

func installTimers(env *config.Env, cfg *config.Config) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating binary: %w", err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return err
	}
	unitDir, err := userUnitDir()
	if err != nil {
		return err
	}
	envArgPath := env.EnvPath
	if !filepath.IsAbs(envArgPath) {
		if abs, err := filepath.Abs(envArgPath); err == nil {
			envArgPath = abs
		}
	}
	envArg := "--env " + envArgPath

	var _ = struct{}{}

	// Shared service unit: runs every due procedure (each timer triggers the
	// same util but scoped to one procedure for OnCalendar precision).
	sharedService := fmt.Sprintf(`[Unit]
Description=homelab-backup scheduled procedure runner

[Service]
Type=oneshot
ExecStart=%s %s run-scheduled
`, bin, envArg)

	servicePath := filepath.Join(unitDir, "homelab-backup-sweeper.service")
	if err := os.WriteFile(servicePath, []byte(sharedService), 0o644); err != nil {
		return err
	}

	// One timer per procedure + one daily offsite push timer (piggybacked in
	// run-scheduled, so a timer for any procedure covers it; use the
	// first procedure's schedule or a standalone 04:00 offsite timer).
	for svcName, svc := range cfg.Services {
		for _, proc := range svc.Procedures {
			c, err := schedule.Parse(proc.Schedule)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", svcName, proc.ID, err)
			}
			uid := unitName(svcName, proc.ID)
			timer := fmt.Sprintf(`[Unit]
Description=homelab-backup %s/%s

[Timer]
OnCalendar=%s
Persistent=true

[Install]
WantedBy=timers.target
`, svcName, proc.ID, strings.Join(c.OnCalendar(), "\nOnCalendar="))
			timerPath := filepath.Join(unitDir, uid+".timer")
			if err := os.WriteFile(timerPath, []byte(timer), 0o644); err != nil {
				return err
			}
		}
	}

	// Fix up the shared service to accept the procedure arg from the timer.
	// (Timers trigger `homelab-backup-<svc>-<proc>.service` by name; create
	// those as links to the shared unit with the procedure pinned.)
	for svcName, svc := range cfg.Services {
		for _, proc := range svc.Procedures {
			uid := unitName(svcName, proc.ID)
			perProc := fmt.Sprintf(`[Unit]
Description=homelab-backup run %s/%s

[Service]
Type=oneshot
ExecStart=%s %s run %s %s
`, svcName, proc.ID, bin, envArg, svcName, proc.ID)
			if err := os.WriteFile(filepath.Join(unitDir, uid+".service"), []byte(perProc), 0o644); err != nil {
				return err
			}
		}
	}

	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	// Enable timers, disable stale ones from removed procedures.
	return enableTimers(cfg, unitDir)
}

func enableTimers(cfg *config.Config, unitDir string) error {
	want := map[string]bool{
		"homelab-backup-sweeper": true,
	}
	for svcName, svc := range cfg.Services {
		for _, proc := range svc.Procedures {
			want[unitName(svcName, proc.ID)] = true
		}
	}
	// disable and delete stale units from removed procedures
	entries, _ := os.ReadDir(unitDir)
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".timer")
		name = strings.TrimSuffix(name, ".service")
		if !strings.HasPrefix(e.Name(), "homelab-backup-") {
			continue
		}
		uid := strings.TrimPrefix(name, "homelab-backup-")
		isMain := e.Name() == "homelab-backup-sweeper.service" || e.Name() == "homelab-backup.service"
		wanted := want[name] || isMain
		if !wanted {
			_ = exec.Command("systemctl", "--user", "disable", "--now", e.Name()).Run()
			_ = os.Remove(filepath.Join(unitDir, e.Name()))
		}
		_ = uid // uid available if finer checks needed
	}
	for _, u := range wantedTimers(cfg) {
		timer := u + ".timer"
		_ = exec.Command("systemctl", "--user", "disable", timer).Run()
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", timer).CombinedOutput(); err != nil {
			return fmt.Errorf("enabling %s: %v\n%s", timer, err, out)
		}
	}
	return nil
}

func wantedTimers(cfg *config.Config) []string {
	var out []string
	for svcName, svc := range cfg.Services {
		for _, proc := range svc.Procedures {
			out = append(out, unitName(svcName, proc.ID))
		}
	}
	return out
}

func unitName(svcName, procID string) string {
	// assume already-id characters (validated by careful naming in yaml)
	return "homelab-backup-" + strings.ReplaceAll(svcName+"-"+procID, "_", "-")
}

func userUnitDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

var _ = config.Load
