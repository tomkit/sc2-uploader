package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the uploader's memory, kept in state.json in the state dir:
// when it started watching (older replays aren't uploaded unless asked),
// every file it has handled by SHA-256, and the optional account link.
type State struct {
	mu sync.Mutex `json:"-"`

	// Replays last modified before this are the backlog: skipped unless
	// `sc2-uploader backfill N` asks for them.
	Since time.Time `json:"since"`
	// SHA-256 of the file → what happened to it.
	Uploads map[string]UploadRecord `json:"uploads"`
	// Files already handled, by path: size+mtime+hash, so rescans and
	// restarts don't re-read or re-hash them.
	Files map[string]FileEntry `json:"files"`
	// Backfill uploads done today (UTC), to stay polite.
	BackfillDay   string `json:"backfillDay,omitempty"`
	BackfillCount int    `json:"backfillCount,omitempty"`
	// Account link (device sign-in); uploads are claimed with it.
	Token          string    `json:"token,omitempty"`
	TokenExpiresAt time.Time `json:"tokenExpiresAt,omitempty"`
}

type FileEntry struct {
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
	Hash string    `json:"hash"`
}

type UploadRecord struct {
	File      string    `json:"file"`
	ReplayID  string    `json:"replayId,omitempty"`
	URL       string    `json:"url,omitempty"`
	At        time.Time `json:"at"`
	Duplicate bool      `json:"duplicate,omitempty"`
	Claimed   bool      `json:"claimed,omitempty"`
	// "new" (played while the uploader ran) or "backfill".
	Source string `json:"source,omitempty"`
	// Rejected by the site as not a valid replay: never retried.
	Rejected string `json:"rejected,omitempty"`
}

func statePath() string { return filepath.Join(stateDir(), "state.json") }

func loadState() (*State, error) {
	s := &State{Uploads: map[string]UploadRecord{}, Files: map[string]FileEntry{}}
	data, err := os.ReadFile(statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.Uploads == nil {
		s.Uploads = map[string]UploadRecord{}
	}
	if s.Files == nil {
		s.Files = map[string]FileEntry{}
	}
	return s, nil
}

// save writes atomically (temp file + rename) with owner-only permissions:
// the file can hold an account token.
func (s *State) save() error {
	s.mu.Lock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}

func (s *State) handled(hash string) (UploadRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.Uploads[hash]
	return r, ok
}

func (s *State) record(hash string, r UploadRecord) {
	s.mu.Lock()
	s.Uploads[hash] = r
	s.mu.Unlock()
}

func (s *State) file(path string) (FileEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.Files[path]
	return e, ok
}

func (s *State) markFile(path string, info os.FileInfo, hash string) {
	s.mu.Lock()
	s.Files[path] = FileEntry{Size: info.Size(), Mod: info.ModTime(), Hash: hash}
	s.mu.Unlock()
}

func (s *State) backfillAllowed(perDay int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().UTC().Format("2006-01-02")
	if s.BackfillDay != today {
		s.BackfillDay, s.BackfillCount = today, 0
	}
	return s.BackfillCount < perDay
}

func (s *State) countBackfill() {
	s.mu.Lock()
	s.BackfillCount++
	s.mu.Unlock()
}

func (s *State) token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Token == "" || (!s.TokenExpiresAt.IsZero() && time.Now().After(s.TokenExpiresAt)) {
		return ""
	}
	return s.Token
}
