// Restic snapshot access for the web UI: list snapshots, stream a single
// stored file (restic dump) or a whole snapshot as tar.gz (restore + tar).
package runner

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"homelab-backup/internal/config"
)

// SnapInfo is one restic snapshot.
type SnapInfo struct {
	ID       string   `json:"id"`
	ShortID  string   `json:"short_id"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Paths    []string `json:"paths"`
}

// Short returns the short snapshot id (falling back to the full id).
func (s SnapInfo) Short() string {
	if s.ShortID != "" {
		return s.ShortID
	}
	if len(s.ID) > 8 {
		return s.ID[:8]
	}
	return s.ID
}

// TimeFmt renders the snapshot time for the UI.
func (s SnapInfo) TimeFmt() string {
	if t, err := timeParse(s.Time); err == nil {
		return t.Format("2006-01-02 15:04:05")
	}
	return s.Time
}

func timeParse(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

// snapshots filter flags are ANDed by restic.
func Snapshots(env *config.Env, cfg *config.Config, svc, proc string) ([]SnapInfo, error) {
	out, err := resticOut(env, cfg, "snapshots", "--json", "--tag", "service:"+svc, "--tag", "procedure:"+proc)
	if err != nil {
		return nil, err
	}
	var snaps []SnapInfo
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("parsing snapshot list: %w", err)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time > snaps[j].Time })
	return snaps, nil
}

var snapRe = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// ResolveSnapshot picks the requested snapshot (validated short/full id)
// or the newest one when pref is empty.
func ResolveSnapshot(env *config.Env, cfg *config.Config, svc, proc, pref string) (SnapInfo, error) {
	snaps, err := Snapshots(env, cfg, svc, proc)
	if err != nil {
		return SnapInfo{}, err
	}
	if len(snaps) == 0 {
		return SnapInfo{}, fmt.Errorf("no snapshots yet for %s/%s", svc, proc)
	}
	if pref != "" {
		if !snapRe.MatchString(pref) {
			return SnapInfo{}, fmt.Errorf("invalid snapshot id")
		}
		for _, s := range snaps {
			if s.ID == pref || s.ShortID == pref {
				return s, nil
			}
		}
		return SnapInfo{}, fmt.Errorf("snapshot %s not found for %s/%s", pref, svc, proc)
	}
	return snaps[0], nil
}

func resticOut(env *config.Env, cfg *config.Config, args ...string) ([]byte, error) {
	cmd := exec.Command(resticBin(), args...)
	cmd.Env = resticEnv(env, cfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("restic %s: %v\n%s", strings.Join(args, " "), err, tail(out))
	}
	return out, nil
}

// LsEntry is one node listed by `restic ls --json` (top-level entries).
type LsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // file | dir | ...
	Size int64  `json:"size"`
}

// TopEntries lists the root-level entries of a snapshot (capped for the UI).
func TopEntries(env *config.Env, cfg *config.Config, snapID string) ([]LsEntry, error) {
	out, err := resticOut(env, cfg, "ls", "--json", snapID, "/")
	if err != nil {
		return nil, err
	}
	var entries []LsEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 4*1024*1024)
	for sc.Scan() {
		var m struct {
			MessageType string `json:"message_type"`
			Name        string `json:"name"`
			Path        string `json:"path"`
			Type        string `json:"type"`
			Size        int64  `json:"size"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.MessageType != "node" {
			continue
		}
		p := strings.Trim(m.Path, "/")
		if p == "" || strings.Contains(p, "/") {
			continue // nested deeper than root level
		}
		entries = append(entries, LsEntry{Name: m.Name, Path: m.Path, Type: m.Type, Size: m.Size})
		if len(entries) >= 500 {
			break
		}
	}
	return entries, nil
}

// DumpStream is a running `restic dump snap file` whose stdout can be read
// as a stream (probed for the first byte before response headers are set).
type DumpStream struct {
	pr   *io.PipeReader
	errc <-chan error
	errb *bytes.Buffer
}

// StartDump launches `restic dump snapID file`, streaming stdout.
func StartDump(env *config.Env, cfg *config.Config, snapID, file string) (*DumpStream, error) {
	if strings.ContainsAny(file, "\n\r") || strings.Contains(file, "..") {
		return nil, fmt.Errorf("invalid file path")
	}
	pr, pw := io.Pipe()
	errb := &bytes.Buffer{}
	cmd := exec.Command(resticBin(), "dump", snapID, file)
	cmd.Env = resticEnv(env, cfg)
	cmd.Stdout = pw
	cmd.Stderr = errb
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting restic dump: %w", err)
	}
	errc := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.CloseWithError(err) // EOF to readers; error delivered via errc
		if err != nil {
			err = fmt.Errorf("restic dump %s: %v\n%s", snapID, err, tail(errb.Bytes()))
		}
		errc <- err
	}()
	return (&DumpStream{pr: pr, errc: errc, errb: errb}), nil
}

// Read passes snapshot content through to the caller.
func (d *DumpStream) Read(p []byte) (int, error) { return d.pr.Read(p) }

// Wait returns the dump process result (call after EOF).
func (d *DumpStream) Wait() error { return <-d.errc }

// DownloadTarGz restores the snapshot into a temp dir below the state dir
// and streams a tar.gz of it. Needs free disk space for the restore copy.
func DownloadTarGz(env *config.Env, cfg *config.Config, w io.Writer, svc, proc string, snap SnapInfo) error {
	tmpRoot := filepath.Join(env.StateDir, "download-"+svc+"-"+proc)
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(tmpRoot, snap.Short()+"-*")
	if err != nil {
		return fmt.Errorf("creating restore dir: %w", err)
	}
	defer os.RemoveAll(tmpRoot)
	if _, err := resticOut(env, cfg, "restore", snap.ID, "--target", tmp); err != nil {
		return err
	}
	return tarGzDir(tmp, w)
}

// tarGzDir streams a gzipped tar of dir without buffering it in memory.
func tarGzDir(root string, w io.Writer) error {
	pr, pw := io.Pipe()
	gz := gzip.NewWriter(pw)
	tw := tar.NewWriter(gz)
	go func() {
		var walkErr error
		walkErr = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		tw.Close()
		gz.Close()
		pw.CloseWithError(walkErr)
	}()
	_, err := io.Copy(w, pr)
	if err != nil {
		pr.Close()
		return err
	}
	return pr.Close()
}
