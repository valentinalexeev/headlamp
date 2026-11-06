package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Syncer is the records-sync agent: it runs next to headscale, pulls the record list from node-control and writes
// headscale's dns.extra_records_path file. headscale reloads that file on change. The agent decides nothing: it
// writes what node-control sends, after checking each record is a name under the base domain at a tailnet address.
type Syncer struct {
	URL        string        // node-control admin listener, for example http://node-control
	Path       string        // the extra_records_path file headscale reads
	BaseDomain string        // records outside it are dropped
	Every      time.Duration // poll interval
	Client     *http.Client  // dials over the tailnet
	Log        *slog.Logger

	applied int64
	lastErr string
}

// recordJSON is one entry of headscale's extra records file.
type recordJSON struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Once pulls the list and, if its version is new, writes the file. It reports the result to node-control in all
// cases, so the console shows a stuck agent.
func (s *Syncer) Once(ctx context.Context) error {
	err := s.pull(ctx)
	if err != nil {
		s.lastErr = err.Error()
		s.Log.Warn("serve-sync: update failed", "err", err)
	} else {
		s.lastErr = ""
	}
	s.report(ctx)
	return err
}

func (s *Syncer) pull(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/serve/v1/records", nil)
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node-control answered %d (is this node tagged for serve-sync?)", resp.StatusCode)
	}
	var got recordsResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&got); err != nil {
		return fmt.Errorf("record list: %w", err)
	}
	if got.Version == s.applied && s.applied != 0 {
		return nil
	}
	pol := Policy{BaseDomain: s.BaseDomain}
	out := []recordJSON{}
	for _, r := range got.Records {
		n := NormalizeName(r.Name)
		if !inBase(n, NormalizeName(pol.BaseDomain)) {
			s.Log.Warn("serve-sync: dropped a record outside the base domain", "name", r.Name)
			continue
		}
		rec, err := RecordFor(n, r.IP)
		if err != nil || rec.Type != r.Type {
			s.Log.Warn("serve-sync: dropped a record that is not at a tailnet address", "name", r.Name)
			continue
		}
		out = append(out, recordJSON{Name: rec.Name, Type: rec.Type, Value: rec.IP.String()})
	}
	if err := writeAtomic(s.Path, out); err != nil {
		return err
	}
	s.applied = got.Version
	s.Log.Info("serve-sync: records written", "version", got.Version, "records", len(out))
	return nil
}

func inBase(n, base string) bool {
	return base != "" && len(n) > len(base)+1 && n[len(n)-len(base)-1:] == "."+base
}

// writeAtomic writes the file next to its target and renames it, so headscale never reads half a file.
func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".extra-records-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Syncer) report(ctx context.Context) {
	b, _ := json.Marshal(syncState{Version: s.applied, Applied: time.Now().UTC(), Error: s.lastErr})
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/serve/v1/state", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := s.Client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// Run calls Once every s.Every until ctx ends. A failure is retried at the next tick; the file keeps its last good
// content meanwhile, so headscale keeps serving the old names rather than none.
func (s *Syncer) Run(ctx context.Context) error {
	if s.URL == "" || s.Path == "" || s.BaseDomain == "" {
		return errors.New("serve-sync: SERVE_SYNC_URL, SERVE_RECORDS_PATH and SERVE_BASE_DOMAIN are required")
	}
	if s.Every <= 0 {
		s.Every = 15 * time.Second
	}
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		_ = s.Once(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
