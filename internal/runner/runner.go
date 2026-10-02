// Package runner executes backup procedures: restic file backups, live
// Postgres dumps through podman exec, and restic retention.
package runner

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"homelab-backup/internal/config"
	"homelab-backup/internal/store"
)

// summary of the most recent restic backup in this process.
var lastSummary struct {
	Bytes    int64
	Files    int64
	Warnings []string
}

func resticEnv(env *config.Env, cfg *config.Config) []string {
	return append(os.Environ(),
		"RESTIC_REPOSITORY="+cfg.Defaults.ResticRepo,
		"RESTIC_PASSWORD_FILE="+filepath.Join(env.StateDir, "restic-pass"),
	)
}

// EnsureRepo initializes the restic repo (and the password file) if missing.
func EnsureRepo(env *config.Env, cfg *config.Config) error {
	if err := os.MkdirAll(cfg.Defaults.ResticRepo, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(env.StateDir, 0o755); err != nil {
		return err
	}
	passFile := filepath.Join(env.StateDir, "restic-pass")
	if _, err := os.Stat(passFile); os.IsNotExist(err) {
		pass, err := randHex(24)
		if err != nil {
			return err
		}
		if err := os.WriteFile(passFile, []byte(pass+"\n"), 0o600); err != nil {
			return err
		}
	}
	if err := restic(env, cfg, "snapshots", "--latest", "1"); err != nil {
		fmt.Println("restic repository not found, initializing…")
		return restic(env, cfg, "init")
	}
	return nil
}

// Run executes one procedure end-to-end and records the outcome.
func Run(env *config.Env, cfg *config.Config, svcName string, svc *config.Service, proc *config.Procedure) error {
	started := time.Now()
	run := store.Run{
		Service:   svcName,
		Procedure: proc.ID,
		Type:      proc.Type,
		StartedAt: started,
		Status:    "success",
	}
	lastSummary.Warnings = nil
	err := execute(env, cfg, svcName, svc, proc)
	run.Duration = time.Since(started).Seconds()
	if err != nil {
		// record the real outcome (previously only local vars were set,
		// leaving failed runs recorded as "success")
		run.Status = "error"
		run.Message = err.Error()
	} else {
		run.Bytes, run.Files = lastSummary.Bytes, lastSummary.Files
		if n := len(lastSummary.Warnings); n > 0 {
			run.Message = strings.Join(lastSummary.Warnings, "\n")
		}
		run.SnapshotID = latestSnapshot(env, cfg, svcName, proc.ID)
	}
	if appendErr := store.Append(env.StateDir, run); appendErr != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not record run: %v\n", appendErr)
	}
	if err != nil {
		return err
	}
	return applyRetention(env, cfg)
}

func execute(env *config.Env, cfg *config.Config, svcName string, svc *config.Service, proc *config.Procedure) error {
	if err := EnsureRepo(env, cfg); err != nil {
		return err
	}
	switch proc.Type {
	case "files":
		if len(proc.Paths) == 0 {
			return fmt.Errorf("procedure %s/%s: files type requires paths", svcName, proc.ID)
		}
		args := []string{"backup", "--json",
			"--tag", "service:" + svcName, "--tag", "procedure:" + proc.ID,
		}
		args = append(args, proc.Paths...)
		return backupRun(env, cfg, nil, args...)

	case "postgres_dump":
		if proc.Container == "" || proc.DBUser == "" {
			return fmt.Errorf("procedure %s/%s: postgres_dump requires container and db_user", svcName, proc.ID)
		}
		svcEnv := svc.ServiceEnv(env, svcName)
		if e := svcEnv["__load_error__"]; e != "" {
			return fmt.Errorf("cannot load service env for %s: %s", svcName, e)
		}
		pw := ""
		if proc.DBPassEnv != "" {
			pw = svcEnv[proc.DBPassEnv]
			if pw == "" {
				return fmt.Errorf("service env for %s has no %s", svcName, proc.DBPassEnv)
			}
		} // pw == "" is allowed: instances using trust auth don't need a password
		containerCmd, err := detectContainerCmd()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(env.StateDir, 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(env.StateDir, "dump-"+svcName+"-"+proc.ID+"-*.sql.gz")
		if err != nil {
			return err
		}
		defer func() { tmp.Close(); os.Remove(tmp.Name()) }()

		// pg_dumpall inside the container piped through gzip
		dumpArgs := []string{"exec"}
		if pw != "" {
			dumpArgs = append(dumpArgs, "-e", "PGPASSWORD="+pw)
		}
		dumpArgs = append(dumpArgs, proc.Container, "pg_dumpall", "-c", "-U", proc.DBUser)
		dump := exec.Command(containerCmd, dumpArgs...)
		gz := exec.Command("gzip")
		pr, pwRdr := io.Pipe()
		dump.Stdout = pwRdr
		gz.Stdin = pr
		gz.Stdout = tmp
		if err := dump.Start(); err != nil {
			return fmt.Errorf("starting dump: %w", err)
		}
		if err := gz.Start(); err != nil {
			return fmt.Errorf("starting gzip: %w", err)
		}
		dumpErr := dump.Wait()
		pwRdr.Close()
		gzErr := gz.Wait()
		if dumpErr != nil {
			return fmt.Errorf("pg_dumpall in %s: %v", proc.Container, dumpErr)
		}
		if gzErr != nil {
			return fmt.Errorf("gzip: %w", gzErr)
		}
		fi, err := tmp.Stat()
		if err != nil || fi.Size() < 64 {
			return fmt.Errorf("dump from %s produced no data", proc.Container)
		}
		// feed the dump into restic via stdin
		f, err := os.Open(tmp.Name())
		if err != nil {
			return err
		}
		defer f.Close()
		args := []string{"backup", "--json",
			"--tag", "service:" + svcName, "--tag", "procedure:" + proc.ID, "--tag", "db_dump",
			"--stdin", "--stdin-filename", svcName+"-"+proc.ID+".sql.gz",
		}
		return backupRun(env, cfg, f, args...)

	default:
		return fmt.Errorf("unknown procedure type %q for %s/%s", proc.Type, svcName, proc.ID)
	}
}

// backupRun executes `restic backup` with the supplied args (optionally
// piping stdin) and parses the JSON summary from stderr (restic emits
// progress JSON there with --json).
func backupRun(env *config.Env, cfg *config.Config, stdin io.Reader, args ...string) error {
	cmd := exec.Command(resticBin(), args...)
	cmd.Env = resticEnv(env, cfg)
	cmd.Stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if bio, er := cmd.StderrPipe(); er == nil {
		go io.Copy(io.Discard, bio)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	parseSummaries(stdout)
	if err := cmd.Wait(); err != nil {
		// restic exit 3 = completed with some unreadable source files
		// (e.g. container-owned UIDs). That's a warning, not a failure.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
			lastSummary.Warnings = append(lastSummary.Warnings,
				"restic completed with warnings (some source files unreadable)")
			return nil
		}
		return fmt.Errorf("restic %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// parseSummaries scans restic --json stderr for the final summary message and
// stores data_added / total_files_processed in lastSummary.
func parseSummaries(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 16*1024*1024)
	for sc.Scan() {
		var m struct {
			MessageType         string `json:"message_type"`
			DataAdded           int64  `json:"data_added"`
			TotalFilesProcessed int64  `json:"total_files_processed"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.MessageType == "summary" {
			lastSummary.Bytes = m.DataAdded
			lastSummary.Files = m.TotalFilesProcessed
		}
	}
}

func applyRetention(env *config.Env, cfg *config.Config) error {
	r := cfg.Defaults.Retention
	if r.KeepDaily == 0 && r.KeepWeekly == 0 {
		return nil
	}
	args := []string{"forget",
		"--keep-daily", strconv.Itoa(orDefault(r.KeepDaily, 7)),
		"--keep-weekly", strconv.Itoa(orDefault(r.KeepWeekly, 4)),
	}
	if err := restic(env, cfg, args...); err != nil {
		return fmt.Errorf("retention (restic forget): %w", err)
	}
	// Prune weekly only, to keep rotations cheap.
	if time.Now().Weekday() == time.Sunday {
		return restic(env, cfg, "prune")
	}
	return nil
}

// latestSnapshot returns the newest snapshot id for a service/procedure
// (best-effort; empty string on any error so runs still record).
func latestSnapshot(env *config.Env, cfg *config.Config, svc, proc string) string {
	out, err := resticOut(env, cfg, "snapshots", "--json", "--tag", "service:"+svc, "--tag", "procedure:"+proc)
	if err != nil {
		return ""
	}
	var snaps []struct {
		ID   string `json:"id"`
		Time string `json:"time"`
	}
	if json.Unmarshal(out, &snaps) != nil || len(snaps) == 0 {
		return ""
	}
	newest := snaps[0]
	for _, s := range snaps[1:] {
		if s.Time > newest.Time {
			newest = s
		}
	}
	return newest.ID
}

func resticBin() string {
	if p, err := exec.LookPath("restic"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, cand := range []string{
			filepath.Join(home, ".local", "bin", "restic"),
			"/usr/local/bin/restic",
			"/usr/bin/restic",
		} {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
	}
	return "restic"
}

func restic(env *config.Env, cfg *config.Config, args ...string) error {
	cmd := exec.Command(resticBin(), args...)
	cmd.Env = resticEnv(env, cfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// "already initialized" is fine when we re-init a fresh repo dir
		if strings.Contains(string(out), "already initialized") {
			return nil
		}
		return fmt.Errorf("restic %s: %v\n%s", strings.Join(args, " "), err, tail(out))
	}
	return nil
}

func detectContainerCmd() (string, error) {
	for _, c := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("neither podman nor docker found in PATH")
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func tail(out []byte) string {
	s := string(out)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return strings.TrimSpace(s)
}

func orDefault(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}
