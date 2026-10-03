// Package offsite pushes the restic repository to one or more remote hosts
// via rsync. The destination list comes from config.OffsiteTargets: either
// the explicit offsite.targets list or the legacy single host/path pair.
package offsite

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"homelab-backup/internal/config"
	"homelab-backup/internal/store"
)

// defaultRsyncFlags is used when a target declares no flags.
func defaultRsyncFlags() []string {
	return []string{"-az", "--partial", "--delete", "--info=stats1"}
}

// Push copies the restic repo to every configured rsync target. Skips
// (informing the caller of the reason) when disabled or unconfigured.
func Push(env *config.Env, cfg *config.Config) (string, error) {
	if !cfg.Offsite.Enabled {
		return "offsite sync disabled in config, skipped", nil
	}
	targets := cfg.OffsiteTargets(env)
	if len(targets) == 0 {
		return "no rsync targets configured, skipped", nil
	}
	var msgs, fails []string
	for _, t := range targets {
		msg, err := pushOne(env, cfg, t)
		msgs = append(msgs, msg)
		if err != nil {
			fails = append(fails, t.Name+": "+err.Error())
		}
	}
	if len(fails) > 0 {
		return strings.Join(msgs, "\n"),
			fmt.Errorf("%d of %d rsync target(s) failed:\n%s", len(fails), len(targets), strings.Join(fails, "\n"))
	}
	return strings.Join(msgs, "\n"), nil
}

// pushOne rsyncs the repo to a single destination and records the outcome.
func pushOne(env *config.Env, cfg *config.Config, t config.OffsiteTarget) (string, error) {
	started := time.Now()
	src := cfg.Defaults.ResticRepo
	dst := t.Destination()

	args := []string{}
	if t.SSHKey != "" {
		args = append(args, "-e", fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes", t.SSHKey))
	}
	flags := strings.Fields(t.Flags)
	if len(flags) == 0 {
		flags = defaultRsyncFlags()
	}
	args = append(args, flags...)
	args = append(args, ensureSlash(src), dst)

	cmd := exec.Command("rsync", args...)
	out, cmdErr := cmd.CombinedOutput()

	procName := "rsync"
	if t.Name != "" && t.Name != "primary" {
		procName = "rsync:" + t.Name
	}
	status, msg := "success", ""
	if cmdErr != nil {
		status = "error"
		msg = fmt.Sprintf("rsync to %s failed: %v\n%s", dst, cmdErr, strings.TrimSpace(string(out)))
	}
	_ = store.Append(env.StateDir, store.Run{
		Service: "offsite", Procedure: procName,
		Type: "rsync", StartedAt: started,
		Duration: time.Since(started).Seconds(), Status: status, Message: msg,
	})
	if cmdErr != nil {
		return msg, cmdErr
	}
	return "pushed restic repo → " + dst, nil
}

// ensureSlash keeps the trailing slash on the local source path so the repo
// contents (not the repo dir itself) are synced.
func ensureSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

var _ = os.Getenv
var _ = filepath.Join
