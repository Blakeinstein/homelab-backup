// Package offsite pushes the restic repository to a remote host via rsync.
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

// Push copies the restic repo to the offsite host. Skips (informing the
// caller of the reason) when disabled or unconfigured.
func Push(env *config.Env, cfg *config.Config) (string, error) {
	host := cfg.Offsite.Host
	path := cfg.Offsite.Path
	enabled := cfg.Offsite.Enabled
	if host == "" {
		host = env.OffsiteHost
		path = env.OffsitePath
	}
	if !enabled || host == "" || path == "" {
		return "offsite sync disabled or unconfigured, skipped", nil
	}

	started := time.Now()
	src := cfg.Defaults.ResticRepo
	dst := fmt.Sprintf("%s%s:%s", userPart(cfg), host, path)
	args := strings.Fields(cfg.Offsite.Flags)
	if len(args) == 0 {
		args = []string{"-az", "--partial", "--delete", "--info=stats1"}
	}
	cmd := exec.Command("rsync", append(args, ensureSlash(src), dst)...)
	out, err := cmd.CombinedOutput()
	status, msg := "success", ""
	if err != nil {
		status = "error"
		msg = fmt.Sprintf("rsync failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	_ = store.Append(env.StateDir, store.Run{
		Service: "offsite", Procedure: "rsync",
		Type: "rsync", StartedAt: started,
		Duration: time.Since(started).Seconds(), Status: status, Message: msg,
	})
	if err != nil {
		return msg, err
	}
	return "pushed restic repo to " + dst, nil
}

// ensureSlash keeps the trailing slash on the local source path so the repo
// contents (not the repo dir itself) are synced.
func ensureSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

// userPart returns the SSH user@(if configured).
func userPart(cfg *config.Config) string {
	if cfg.Offsite.SSHUser != "" {
		return cfg.Offsite.SSHUser + "@"
	}
	return ""
}

var _ = os.Getenv
var _ = filepath.Join
