// Package store persists backup run history as JSON Lines in the state
// directory — one record per procedure run, newest last.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run is one execution of a procedure.
type Run struct {
	Service    string    `json:"service"`
	Procedure  string    `json:"procedure"`
	Type       string    `json:"type"`
	StartedAt  time.Time `json:"started_at"`
	Duration   float64   `json:"duration_s"`
	Status     string    `json:"status"` // success | error | running
	Message    string    `json:"message,omitempty"`
	Bytes      int64     `json:"bytes_added"`
	Files      int64     `json:"files_processed"`
	SnapshotID string    `json:"snapshot_id,omitempty"` // restic snapshot of this run
}

var mu sync.Mutex

// Append a run record to the JSONL history and mirror failures to the log.
func Append(stateDir string, run Run) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "runs.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	f.Write(append(b, '\n'))
	return nil
}

// Load returns the most recent `limit` runs (newest first), and the latest
// run for each service/procedure pair.
func Load(stateDir string, limit int) ([]Run, map[string]Run, error) {
	data, err := os.Open(filepath.Join(stateDir, "runs.jsonl"))
	if os.IsNotExist(err) {
		return nil, map[string]Run{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer data.Close()

	var all []Run
	sc := bufio.NewScanner(data)
	sc.Buffer(nil, 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Run
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // skip corrupted lines
		}
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].StartedAt.After(all[j].StartedAt) })

	latest := map[string]Run{}
	for _, r := range all {
		key := r.Service + "/" + r.Procedure
		if _, ok := latest[key]; !ok && r.Status != "running" {
			latest[key] = r
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, latest, nil
}

func key(r Run) string { return fmt.Sprintf("%s/%s", r.Service, r.Procedure) }
