// Package offsite pushes the restic repository to one or more remote
// destinations via rsync (ssh targets) or rclone (gdrive/seedbox/other
// cloud remotes). Destinations come from config.OffsiteTargets: either
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

// pushOne syncs the repo to a single destination (rsync over ssh, or
// any rclone remote when target.Remote is set) and records the outcome.
func pushOne(env *config.Env, cfg *config.Config, t config.OffsiteTarget) (string, error) {
	started := time.Now()
	src := cfg.Defaults.ResticRepo
	prog := "rsync"
	var label string

	record := func(status, msg string, err error) (string, error) {
		procName := prog
		if t.Name != "" && t.Name != "primary" {
			procName = prog + ":" + t.Name
		}
		_ = store.Append(env.StateDir, store.Run{
			Service: "offsite", Procedure: procName,
			Type: prog, StartedAt: started,
			Duration: time.Since(started).Seconds(), Status: status, Message: msg,
		})
		if err != nil {
			return msg, err
		}
		return msg, nil
	}

	var out *exec.Cmd
	if t.Remote == "" {
		label = t.Destination()
		var rsyncArgs []string
		if t.SSHKey != "" {
			rsyncArgs = append(rsyncArgs, "-e", fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes", t.SSHKey))
		}
		flags := strings.Fields(t.Flags)
		if len(flags) == 0 {
			flags = defaultRsyncFlags()
		}
		rsyncArgs = append(rsyncArgs, flags...)
		rsyncArgs = append(rsyncArgs, ensureSlash(src), label)

		if t.SSHPassword != "" && t.SSHKey == "" {
			// Password auth: rsync runs under sshpass -e with SSHPASS in
			// the child env, so the secret never shows in ps output.
			sshpass, err := exec.LookPath("sshpass")
			if err != nil {
				return record("error", "password auth needs sshpass: sudo apt install sshpass (or use ssh_key)", err)
			}
			out = exec.Command(sshpass, append([]string{"-e", "rsync"}, rsyncArgs...)...)
			out.Env = append(os.Environ(), "SSHPASS="+t.SSHPassword)
		} else {
			out = exec.Command("rsync", rsyncArgs...)
		}
	} else {
		label = t.Remote
		prog = "rclone"
		out = exec.Command("rclone", "sync", "--delete")
		// custom flags replace nothing; defaults are minimal on purpose
		// (--delete keeps the mirror faithful, rclone retries are built in)
		if f := strings.Fields(t.Flags); len(f) > 0 {
			out.Args = append(out.Args, f...)
		}
		out.Args = append(out.Args, ensureSlash(src), label)
	}

	outBytes, cmdErr := out.CombinedOutput()

	status, msg := "success", ""
	if cmdErr != nil {
		status = "error"
		msg = fmt.Sprintf("%s to %s failed: %v\n%s", prog, label, cmdErr, strings.TrimSpace(string(outBytes)))
	} else {
		msg = "pushed restic repo → " + label
	}
	return record(status, msg, cmdErr)
}

// dstLabel renders the destination tail of the push command for logs.
func dstLabel(prog string, args []string) string {
	if len(args) == 0 {
		return prog
	}
	return args[len(args)-2] + " → " + args[len(args)-1]
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
