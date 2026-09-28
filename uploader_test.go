package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A fake replay: the MPQ user-data header the real files start with.
func fakeReplay(tag string) []byte {
	b := make([]byte, 256)
	copy(b, []byte("MPQ\x1b"))
	copy(b[16:], []byte(tag))
	return b
}

type fakeSite struct {
	srv     *httptest.Server
	uploads atomic.Int32
	mu      sync.Mutex
	hashes  map[string]bool
	names   []string
	mode    atomic.Value    // "", "429", "html", "reject"
	onSite  map[string]bool // hashes /api/replays/exists reports as uploaded
	checks  atomic.Int32
	claimed atomic.Int32
}

func newFakeSite(t *testing.T) *fakeSite {
	f := &fakeSite{hashes: map[string]bool{}, onSite: map[string]bool{}}
	f.mode.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/parse":
			if r.URL.Query().Get("summary") != "1" {
				t.Errorf("upload without ?summary=1")
			}
			if !strings.HasPrefix(r.UserAgent(), "sc2-uploader/") {
				t.Errorf("unexpected User-Agent %q", r.UserAgent())
			}
			switch f.mode.Load().(string) {
			case "429":
				w.Header().Set("Retry-After", "120")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(429)
				io.WriteString(w, `{"error":"Too many uploads"}`)
				return
			case "html":
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(403)
				io.WriteString(w, "<html>Just a moment...</html>")
				return
			case "reject":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"Not a StarCraft II replay file"}`)
				return
			}
			file, hdr, err := r.FormFile("replay")
			if err != nil {
				t.Errorf("no replay field: %v", err)
				return
			}
			data, _ := io.ReadAll(file)
			h := sha256Hex(data)
			f.mu.Lock()
			dup := f.hashes[h]
			f.hashes[h] = true
			f.names = append(f.names, hdr.Filename)
			f.mu.Unlock()
			f.uploads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "11111111-2222-3333-4444-" + h[:12], "url": "https://x/en/replay/" + h[:8], "duplicate": dup})
		case "/api/replays/exists":
			f.checks.Add(1)
			var body struct {
				Hashes []string `json:"hashes"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			found := map[string]any{}
			f.mu.Lock()
			for _, h := range body.Hashes {
				if f.onSite[h] {
					found[h] = map[string]string{"id": "exists-" + h[:8], "url": "https://x/en/replay/exists"}
				}
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"found": found})
		case "/api/replays/claim":
			if r.Header.Get("Authorization") != "Bearer sc2_test" {
				w.WriteHeader(401)
				return
			}
			f.claimed.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true,"claimed":true}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func setup(t *testing.T) (site *fakeSite, dir string) {
	t.Helper()
	site = newFakeSite(t)
	t.Setenv("SC2_API_BASE", site.srv.URL)
	t.Setenv("SC2_UPLOADER_HOME", t.TempDir())
	return site, t.TempDir()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startDaemon(t *testing.T, s *State, dir string, opts Options) (*Uploader, func()) {
	t.Helper()
	u, err := newUploader(s, []string{dir}, opts)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { u.run(stop); close(done) }()
	return u, func() { close(stop); <-done; u.watcher.Close() }
}

var noBackfill = Options{Backfill: false}

func TestUploadsNewReplaysOnlyOnceAndSkipsTheBacklogWhenAsked(t *testing.T) {
	site, dir := setup(t)
	old := filepath.Join(dir, "Old game.SC2Replay")
	os.WriteFile(old, fakeReplay("old"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)

	s, _ := loadState()
	s.Since = time.Now().Add(-time.Minute)
	s.save()
	_, stop := startDaemon(t, s, dir, noBackfill)

	// A game ends: the file appears in two writes, like a real client.
	p := filepath.Join(dir, "Rainfall LE.SC2Replay")
	data := fakeReplay("new game")
	os.WriteFile(p, data[:100], 0o644)
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(p, data, 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644)

	waitFor(t, "the upload", func() bool { return site.uploads.Load() == 1 })
	time.Sleep(3 * time.Second) // settle window: no second upload
	stop()
	if n := site.uploads.Load(); n != 1 {
		t.Fatalf("uploads = %d, want 1 (backlog skipped, no duplicate)", n)
	}
	if site.names[0] != "Rainfall LE.SC2Replay" {
		t.Fatalf("uploaded %q", site.names[0])
	}

	// Restart: the recorded hash means no re-upload.
	s2, _ := loadState()
	if len(s2.Uploads) != 1 {
		t.Fatalf("state has %d uploads, want 1", len(s2.Uploads))
	}
	_, stop2 := startDaemon(t, s2, dir, noBackfill)
	time.Sleep(3 * time.Second)
	stop2()
	if n := site.uploads.Load(); n != 1 {
		t.Fatalf("re-uploaded after restart: %d uploads", n)
	}
}

func TestBackfillUploadsTheMostRecentOlderReplays(t *testing.T) {
	site, dir := setup(t)
	for i, name := range []string{"a", "b", "c"} {
		p := filepath.Join(dir, name+".SC2Replay")
		os.WriteFile(p, fakeReplay(name), 0o644)
		at := time.Now().Add(-time.Duration(3-i) * time.Hour)
		os.Chtimes(p, at, at)
	}
	s, _ := loadState()
	s.Since = time.Now()
	u, _ := newUploader(s, []string{dir}, noBackfill)
	if got := u.backfill(2); got != 2 {
		t.Fatalf("backfill looked at %d", got)
	}
	if site.uploads.Load() != 2 {
		t.Fatalf("uploads = %d, want 2", site.uploads.Load())
	}
	if !strings.HasPrefix(site.names[0], "c") || !strings.HasPrefix(site.names[1], "b") {
		t.Fatalf("backfill order %v, want newest first", site.names)
	}
}

func TestLinkedUploadsAreClaimed(t *testing.T) {
	site, dir := setup(t)
	s, _ := loadState()
	s.Since = time.Now().Add(-time.Minute)
	s.Token = "sc2_test"
	s.TokenExpiresAt = time.Now().Add(time.Hour)
	_, stop := startDaemon(t, s, dir, noBackfill)
	os.WriteFile(filepath.Join(dir, "g.SC2Replay"), fakeReplay("g"), 0o644)
	waitFor(t, "claim", func() bool { return site.claimed.Load() == 1 })
	stop()
}

func TestUploadErrors(t *testing.T) {
	site, dir := setup(t)
	p := filepath.Join(dir, "x.SC2Replay")
	data := fakeReplay("x")

	site.mode.Store("429")
	_, err := upload(p, data)
	var re *RetryError
	if !errors.As(err, &re) || re.After != 120*time.Second {
		t.Fatalf("429: got %v, want a retry after Retry-After", err)
	}

	site.mode.Store("html")
	_, err = upload(p, data)
	if !errors.As(err, &re) || !strings.Contains(re.Reason, "bot protection") || re.After < 30*time.Minute {
		t.Fatalf("html 403: got %v", err)
	}

	site.mode.Store("reject")
	_, err = upload(p, data)
	var rej *RejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("400: got %v, want rejected", err)
	}

	// A rejected file is recorded and never retried.
	site.mode.Store("reject")
	s, _ := loadState()
	s.Since = time.Now().Add(-time.Minute)
	u, _ := newUploader(s, []string{dir}, noBackfill)
	os.WriteFile(p, data, 0o644)
	u.handle(p)
	if rec, ok := s.handled(sha256Hex(data)); !ok || rec.Rejected == "" {
		t.Fatalf("rejection not recorded: %+v", rec)
	}
}

func TestLooksLikeReplay(t *testing.T) {
	if !looksLikeReplay(fakeReplay("x")) {
		t.Fatal("real header refused")
	}
	for _, b := range [][]byte{[]byte("MPQ\x1b"), make([]byte, 100), append([]byte("PK\x03\x04"), make([]byte, 100)...)} {
		if looksLikeReplay(b) {
			t.Fatalf("accepted %q", b[:4])
		}
	}
}

func TestFindsMultiplayerFoldersUnderAccounts(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "891556", "1-S2-1-1240773", "Replays", "Multiplayer")
	os.MkdirAll(want, 0o755)
	os.MkdirAll(filepath.Join(root, "891556", "1-S2-1-1240773", "Replays", "Campaign"), 0o755)
	os.MkdirAll(filepath.Join(root, "891556", "2-S2-1-99", "Replays"), 0o755) // no Multiplayer yet
	got := multiplayerDirs(root)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want [%s]", got, want)
	}
}

func TestPoliteBackfillSkipsWhatTheSiteHasAndRespectsTheDailyCap(t *testing.T) {
	site, dir := setup(t)
	names := []string{"oldest", "middle", "newest"}
	for i, name := range names {
		p := filepath.Join(dir, name+".SC2Replay")
		os.WriteFile(p, fakeReplay(name), 0o644)
		at := time.Now().Add(-time.Duration(3-i) * time.Hour)
		os.Chtimes(p, at, at)
	}
	// Someone already uploaded "newest".
	site.onSite[sha256Hex(fakeReplay("newest"))] = true

	s, _ := loadState()
	s.Since = time.Now()
	_, stop := startDaemon(t, s, dir, Options{Backfill: true, BackfillEvery: 200 * time.Millisecond, BackfillPerDay: 1})
	waitFor(t, "one backfill upload", func() bool { return site.uploads.Load() == 1 })
	time.Sleep(3 * time.Second) // cap reached: nothing more today
	stop()

	if n := site.uploads.Load(); n != 1 {
		t.Fatalf("uploads = %d, want 1 (daily cap)", n)
	}
	if site.names[0] != "middle.SC2Replay" {
		t.Fatalf("uploaded %q first, want the newest one not already on the site", site.names[0])
	}
	if site.checks.Load() != 1 {
		t.Fatalf("hash checks = %d, want 1 batch", site.checks.Load())
	}
	st, _ := loadState()
	rec, ok := st.handled(sha256Hex(fakeReplay("newest")))
	if !ok || !rec.Duplicate || rec.ReplayID == "" {
		t.Fatalf("game already on the site not recorded: %+v", rec)
	}
	if st.BackfillCount != 1 {
		t.Fatalf("backfill count %d", st.BackfillCount)
	}
}

func TestNewGamesGoBeforeTheBacklog(t *testing.T) {
	site, dir := setup(t)
	old := filepath.Join(dir, "old.SC2Replay")
	os.WriteFile(old, fakeReplay("old"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)
	s, _ := loadState()
	s.Since = time.Now().Add(-time.Minute)
	// Backfill would start immediately, but a new game arrives first.
	u, _ := newUploader(s, []string{dir}, Options{Backfill: true, BackfillEvery: 10 * time.Second, BackfillPerDay: 5})
	u.enqueue(filepath.Join(dir, "new.SC2Replay"), 0)
	os.WriteFile(filepath.Join(dir, "new.SC2Replay"), fakeReplay("new"), 0o644)
	u.scan(u.discover())
	u.backfillStep() // a new game is pending: no backfill work
	if site.checks.Load() != 0 || site.uploads.Load() != 0 {
		t.Fatalf("backfill ran while a new game was waiting")
	}
	u.drain()
	if site.uploads.Load() != 1 || site.names[0] != "new.SC2Replay" {
		t.Fatalf("new game not uploaded first: %v", site.names)
	}
	u.watcher.Close()
}

func TestHandledFilesAreNotReHashedOnRescan(t *testing.T) {
	_, dir := setup(t)
	p := filepath.Join(dir, "g.SC2Replay")
	os.WriteFile(p, fakeReplay("g"), 0o644)
	s, _ := loadState()
	s.Since = time.Now().Add(-time.Hour)
	u, _ := newUploader(s, []string{dir}, noBackfill)
	defer u.watcher.Close()
	u.handle(p)
	u.scan([]string{dir})
	u.mu.Lock()
	n := len(u.pending)
	u.mu.Unlock()
	if n != 0 {
		t.Fatalf("rescan re-queued a handled file")
	}
}
