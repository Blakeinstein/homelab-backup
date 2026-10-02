// Marker files and offsite gating for scheduled runs.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"homelab-backup/internal/config"
	"homelab-backup/internal/offsite"
	"homelab-backup/internal/schedule"
)

// due reports whether the cron schedule c fired between the last recorded
// execution (marker mtime) and now. c.Next(from) returns the first matching
// minute strictly after `from`, so one call answers the question.
func due(env *config.Env, svcName, procID string, c *schedule.Cron, now time.Time) bool {
	last := readLastRun(env.StateDir, svcName, procID)
	if last.IsZero() {
		// never ran: treat as due if a fire happened within the last day
		last = now.Add(-24 * time.Hour)
	}
	next := c.Next(last)
	return !next.After(now)
}

func readLastRun(stateDir, svcName, procID string) time.Time {
	dir := filepath.Join(stateDir, "sched")
	fi, err := os.Stat(filepath.Join(dir, svcName+"-"+procID+".ts"))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func touchMarker(env *config.Env, svcName, procID string, t time.Time) {
	dir := filepath.Join(env.StateDir, "sched")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f := filepath.Join(dir, svcName+"-"+procID+".ts")
	if err := os.Chtimes(f, t, t); err != nil {
		_ = os.WriteFile(f, nil, 0o644)
		_ = os.Chtimes(f, t, t)
	}
}

// offsite gate: push at most once every OFFSITE_MIN_HOURS (default 20h).
var offsiteMu sync.Mutex

func offsiteMaybe(env *config.Env, cfg *config.Config) error {
	host := cfg.Offsite.Host
	if host == "" {
		host = env.OffsiteHost
	}
	if !cfg.Offsite.Enabled || host == "" {
		return nil
	}
	offsiteMu.Lock()
	defer offsiteMu.Unlock()

	gate := filepath.Join(env.StateDir, "offsite-last.ts")
	if fi, err := os.Stat(gate); err == nil {
		minHours := 20
		if h := env.Values["OFFSITE_MIN_HOURS"]; h != "" {
			fmt.Sscanf(h, "%d", &minHours)
		}
		if time.Since(fi.ModTime()) < time.Duration(minHours)*time.Hour {
			return nil // pushed recently
		}
	}

	msg, err := offsite.Push(env, cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", msg, err)
	}
	fmt.Println(msg)

	if err := os.MkdirAll(filepath.Dir(gate), 0o755); err != nil {
		return nil
	}
	if _, statErr := os.Stat(gate); os.IsNotExist(statErr) {
		_ = os.WriteFile(gate, nil, 0o644)
	}
	_ = os.Chtimes(gate, time.Now(), time.Now())
	return nil
}

var _ = schedule.Parse
