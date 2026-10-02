// homelab-backup is the agent and dashboard for the homelab backup service.
//
// Commands:
//
//	homelab-backup serve                          HTMX dashboard + endpoints (default)
//	homelab-backup run <service> <procedure>      run a single procedure now
//	homelab-backup run-scheduled                  run every procedure whose schedule is due
//	homelab-backup install-timers                 (re)generate systemd user timers from the yaml
//	homelab-backup offsite                        push the restic repo offsite via rsync
//	homelab-backup init                           initialize/check the restic repository
package main

import (
	"fmt"
	"os"
	"time"

	"homelab-backup/internal/config"
	"homelab-backup/internal/offsite"
	"homelab-backup/internal/runner"
	"homelab-backup/internal/schedule"
)

func fatal(err error) {
	if err == nil {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}

func resolve(envPath string) (*config.Env, *config.Config) {
	env, err := config.LoadEnv(envPath)
	if err != nil {
		fatal(err)
	}
	cfg, err := config.Load(env.Values, env.BackupConfigPath)
	if err != nil {
		fatal(err)
	}
	return env, cfg
}

func main() {
	args := os.Args[1:]
	cmd := ""
	rest := make([]string, 0, len(args))
	envPath := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--env" && i+1 < len(args) {
			envPath = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) > 0 && rest[0][0] != '-' {
		cmd = rest[0]
		rest = rest[1:]
	}
	switch cmd {
	case "run":
		if len(rest) != 2 {
			fatal(fmt.Errorf("usage: homelab-backup run <service> <procedure>"))
		}
		env, cfg := resolve(envPath)
		svc, proc := lookup(cfg, rest[0], rest[1])
		if err := runner.Run(env, cfg, rest[0], svc, proc); err != nil {
			fatal(err)
		}
		fmt.Println("backup succeeded:", rest[0]+"/"+rest[1])
	case "run-scheduled":
		env, cfg := resolve(envPath)
		fatal(runScheduled(env, cfg))
	case "install-timers":
		env, cfg := resolve(envPath)
		fatal(installTimers(env, cfg))
	case "offsite":
		env, cfg := resolve(envPath)
		msg, err := offsite.Push(env, cfg)
		if err != nil {
			fatal(err)
		}
		fmt.Println(msg)
	case "init":
		env, cfg := resolve(envPath)
		if err := runner.EnsureRepo(env, cfg); err != nil {
			fatal(err)
		}
		fmt.Println("restic repository ready at", cfg.Defaults.ResticRepo)
	default: // serve
		serve(envPath)
	}
}

// lookup validates service/procedure names and returns pointers into the config.
func lookup(cfg *config.Config, svcName, procID string) (*config.Service, *config.Procedure) {
	svc, ok := cfg.Services[svcName]
	if !ok {
		fatal(fmt.Errorf("unknown service %q", svcName))
	}
	for _, p := range svc.Procedures {
		if p.ID == procID {
			return svc, p
		}
	}
	fatal(fmt.Errorf("unknown procedure %q in service %q", procID, svcName))
	return nil, nil
}

// runScheduled executes procedures whose schedule fired since the last
// recorded execution (marker files track per-procedure last completion).
func runScheduled(env *config.Env, cfg *config.Config) error {
	var failures []string
	now := time.Now()
	for svcName, svc := range cfg.Services {
		for _, proc := range svc.Procedures {
			c, err := schedule.Parse(proc.Schedule)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: bad schedule: %v", svcName, proc.ID, err))
				continue
			}
			if !due(env, svcName, proc.ID, c, now) {
				continue
			}
			fmt.Printf("running %s/%s\n", svcName, proc.ID)
			if err := runner.Run(env, cfg, svcName, svc, proc); err != nil {
				failures = append(failures, svcName+"/"+proc.ID+": "+err.Error())
				continue
			}
			touchMarker(env, svcName, proc.ID, time.Now())
		}
	}
	if err := offsiteMaybe(env, cfg); err != nil {
		failures = append(failures, "offsite: "+err.Error())
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "ERROR:", f)
		}
		return fmt.Errorf("%d scheduled run(s) failed", len(failures))
	}
	return nil
}
