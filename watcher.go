package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var errDropped = errors.New("the OS dropped some file events; rescanning")

// The loop sleeps on the OS change feed (dirWatcher). A slow rescan catches
// anything the feed missed and new accounts or folders. Nothing is hashed
// until a file is new and has stopped changing.
//
// Two queues, new games first:
//   - new: replays written after the uploader started watching. Uploaded
//     as soon as the file settles.
//   - backlog: replays already on disk. Backfilled politely in the
//     background: newest first; hashes checked with the site in batches so
//     games already uploaded (by anyone) cost nothing; then at most one
//     upload per BackfillEvery and BackfillPerDay a day, leaving the rest
//     of the site's 50-a-day allowance for new games; paused while a new
//     game is waiting or the site says to slow down.
const (
	rescanEvery    = 5 * time.Minute
	settleInterval = 2 * time.Second // a file must keep the same size this long
	maxFileBytes   = 8 << 20         // the site's upload limit
	existsBatch    = 50
)

type Options struct {
	Backfill       bool
	BackfillEvery  time.Duration
	BackfillPerDay int
}

var DefaultOptions = Options{Backfill: true, BackfillEvery: 90 * time.Second, BackfillPerDay: 25}

type Uploader struct {
	state    *State
	explicit []string
	opts     Options
	watcher  dirWatcher

	mu       sync.Mutex
	watched  map[string]bool
	pending  map[string]time.Time // new games: file → earliest time to try it
	failures map[string]int
	wake     chan struct{}

	// Backfill.
	backlog       []string // older replays not yet looked at, newest first
	toUpload      []string // checked with the site: not there yet
	nextBackfill  time.Time
	backlogQueued map[string]bool
}

func newUploader(state *State, explicit []string, opts Options) (*Uploader, error) {
	w, err := newDirWatcher()
	if err != nil {
		return nil, err
	}
	return &Uploader{
		state:         state,
		explicit:      explicit,
		opts:          opts,
		watcher:       w,
		watched:       map[string]bool{},
		pending:       map[string]time.Time{},
		failures:      map[string]int{},
		wake:          make(chan struct{}, 1),
		backlogQueued: map[string]bool{},
	}, nil
}

// discover (re)adds watches: each replay folder, plus the Accounts roots
// and account folders so a new region/toon folder is noticed.
func (u *Uploader) discover() []string {
	dirs, roots := watchRoots(u.explicit)
	watchList := append(append([]string{}, roots...), dirs...)
	for _, root := range roots {
		if accounts, err := os.ReadDir(root); err == nil {
			for _, a := range accounts {
				if a.IsDir() {
					watchList = append(watchList, filepath.Join(root, a.Name()))
				}
			}
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, d := range watchList {
		if u.watched[d] {
			continue
		}
		if err := u.watcher.Add(d); err != nil {
			log.Printf("can't watch %s: %v", d, err)
			continue
		}
		u.watched[d] = true
		// FSEvents reports resolved paths (/private/var for /var); match both.
		if real, err := filepath.EvalSymlinks(d); err == nil && real != d {
			u.watched[real] = true
		}
	}
	return dirs
}

// known reports whether a file was already handled, by size+mtime, so
// rescans don't re-read or re-hash anything.
func (u *Uploader) known(path string, info os.FileInfo) bool {
	e, ok := u.state.file(path)
	return ok && e.Size == info.Size() && e.Mod.Equal(info.ModTime())
}

// scan queues new replays and collects the backlog.
func (u *Uploader) scan(dirs []string) {
	var older []struct {
		path string
		mod  time.Time
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isReplayFile(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if u.known(p, info) {
				continue
			}
			if info.ModTime().Before(u.state.Since) {
				older = append(older, struct {
					path string
					mod  time.Time
				}{p, info.ModTime()})
				continue
			}
			u.enqueue(p, 0)
		}
	}
	if !u.opts.Backfill {
		return
	}
	sort.Slice(older, func(i, j int) bool { return older[i].mod.After(older[j].mod) })
	u.mu.Lock()
	added := 0
	for _, o := range older {
		if !u.backlogQueued[o.path] {
			u.backlogQueued[o.path] = true
			u.backlog = append(u.backlog, o.path)
			added++
		}
	}
	u.mu.Unlock()
	if added > 0 {
		log.Printf("backfill: %d older replay(s) to check, newest first (up to %d uploads a day, one every %s)",
			added, u.opts.BackfillPerDay, u.opts.BackfillEvery)
	}
}

func (u *Uploader) enqueue(path string, delay time.Duration) {
	u.mu.Lock()
	at := time.Now().Add(delay)
	if cur, ok := u.pending[path]; !ok || at.Before(cur) || delay > 0 {
		u.pending[path] = at
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *Uploader) run(stop <-chan struct{}) {
	u.scan(u.discover())
	log.Printf("watching %d folder(s); new games upload as they finish", len(u.watched))

	rescan := time.NewTicker(rescanEvery)
	defer rescan.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case p, ok := <-u.watcher.Paths():
			if !ok {
				return
			}
			u.onPath(p)
		case err := <-u.watcher.Errors():
			log.Printf("watch: %v", err)
			u.scan(u.discover())
		case <-rescan.C:
			u.scan(u.discover())
		case <-u.wake:
			u.drain()
		case <-tick.C:
			u.drain()
			u.backfillStep()
		}
	}
}

func (u *Uploader) onPath(p string) {
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		// A new account or region folder: pick up its replay folder.
		u.scan(u.discover())
		return
	}
	if !isReplayFile(p) || !u.inWatchedFolder(p) {
		return
	}
	info, err := os.Stat(p)
	if err != nil || u.known(p, info) {
		return
	}
	// Events fire for old files too (touched, copied in, re-saved). Only
	// games played after the start time are "new"; the rest go through
	// the polite backfill like any other older replay.
	if info.ModTime().Before(u.state.Since) {
		u.mu.Lock()
		if u.opts.Backfill && !u.backlogQueued[p] {
			u.backlogQueued[p] = true
			u.backlog = append(u.backlog, p)
		}
		u.mu.Unlock()
		return
	}
	u.enqueue(p, settleInterval) // let the game finish writing
}

// inWatchedFolder keeps FSEvents' recursive stream to the replay folders
// (not Campaign/Challenge or anything else under Accounts).
func (u *Uploader) inWatchedFolder(p string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.watched[filepath.Dir(p)]
}

func (u *Uploader) drain() {
	now := time.Now()
	u.mu.Lock()
	var due []string
	for p, at := range u.pending {
		if !at.After(now) {
			due = append(due, p)
		}
	}
	u.mu.Unlock()
	sort.Strings(due)
	for _, p := range due {
		u.mu.Lock()
		delete(u.pending, p)
		u.mu.Unlock()
		u.handle(p, "new")
	}
}

// handle uploads one settled file. source is "new" for games played while
// watching and "backfill" otherwise; only new games can be auto-analyzed.
func (u *Uploader) handle(path, source string) {
	// Settled: same size and mtime twice, settleInterval apart.
	a, err := os.Stat(path)
	if err != nil {
		return // gone
	}
	if a.Size() > maxFileBytes {
		log.Printf("skipping %s: larger than the site's 8 MB limit", filepath.Base(path))
		return
	}
	time.Sleep(settleInterval)
	b, err := os.Stat(path)
	if err != nil {
		return
	}
	if a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
		u.enqueue(path, settleInterval)
		return
	}
	data, err := os.ReadFile(path) // fails while another process holds it (Windows)
	if err != nil || !looksLikeReplay(data) {
		u.retry(path, &RetryError{After: 10 * time.Second, Reason: "not readable yet"})
		return
	}
	u.uploadData(path, b, data, source)
}

// uploadData uploads (or skips, if already handled) one settled file.
func (u *Uploader) uploadData(path string, info os.FileInfo, data []byte, source string) bool {
	hash := sha256Hex(data)
	if rec, ok := u.state.handled(hash); ok {
		u.state.markFile(path, info, hash)
		if rec.ReplayID != "" && !rec.Claimed {
			u.claim(hash, rec)
		}
		_ = u.state.save()
		return true
	}
	res, err := upload(path, data)
	if err != nil {
		var rej *RejectedError
		if errors.As(err, &rej) {
			log.Printf("✗ %s: %s", filepath.Base(path), rej.Reason)
			u.state.record(hash, UploadRecord{File: path, At: time.Now(), Rejected: rej.Reason})
			u.state.markFile(path, info, hash)
			_ = u.state.save()
			return true
		}
		u.retry(path, err)
		return false
	}
	u.mu.Lock()
	delete(u.failures, path)
	u.mu.Unlock()
	rec := UploadRecord{File: path, ReplayID: res.ID, URL: res.URL, At: time.Now(), Duplicate: res.Duplicate, Source: source}
	u.state.record(hash, rec)
	u.state.markFile(path, info, hash)
	_ = u.state.save()
	what := "uploaded"
	if res.Duplicate {
		what = "already on the site"
	}
	log.Printf("✓ %s %s → %s", filepath.Base(path), what, res.URL)
	u.claim(hash, rec)
	return true
}

func (u *Uploader) claim(hash string, rec UploadRecord) {
	token := u.state.token()
	if token == "" || rec.ReplayID == "" {
		return
	}
	source := rec.Source
	if source == "" {
		source = "backfill"
	}
	res, err := claim(token, rec.ReplayID, hash, source)
	if err != nil {
		log.Printf("couldn't link %s to your account: %v", filepath.Base(rec.File), err)
		return
	}
	if res.AutoCoach != nil && res.AutoCoach.Queued {
		log.Printf("  queued for an AI Coach analysis (coach subscription)")
	} else if res.AutoCoach != nil && source == "new" && res.AutoCoach.Reason != "not_subscribed" && res.AutoCoach.Reason != "not_your_game" {
		log.Printf("  not auto-analyzed: %s", res.AutoCoach.Reason)
	}
	rec.Claimed = true
	u.state.record(hash, rec)
	_ = u.state.save()
}

// retry backs off: the server's Retry-After when it gave one, otherwise
// doubling from 30 s to 30 min. A rate limit also pauses the backfill.
func (u *Uploader) retry(path string, err error) {
	u.mu.Lock()
	u.failures[path]++
	n := u.failures[path]
	u.mu.Unlock()
	delay := 30 * time.Second << min(n-1, 6)
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	var re *RetryError
	if errors.As(err, &re) && re.After > delay {
		delay = re.After
	}
	u.pauseBackfill(delay)
	if n <= 3 || n%10 == 0 {
		log.Printf("will retry %s in %s: %v", filepath.Base(path), delay.Round(time.Second), err)
	}
	u.enqueue(path, delay)
}

func (u *Uploader) pauseBackfill(d time.Duration) {
	u.mu.Lock()
	if t := time.Now().Add(d); t.After(u.nextBackfill) {
		u.nextBackfill = t
	}
	u.mu.Unlock()
}

// backfillStep does one polite unit of backfill work, if it's time.
func (u *Uploader) backfillStep() {
	if !u.opts.Backfill {
		return
	}
	u.mu.Lock()
	busy := len(u.pending) > 0 || time.Now().Before(u.nextBackfill)
	u.mu.Unlock()
	if busy {
		return
	}
	if !u.state.backfillAllowed(u.opts.BackfillPerDay) {
		return
	}

	u.mu.Lock()
	var next string
	if len(u.toUpload) > 0 {
		next, u.toUpload = u.toUpload[0], u.toUpload[1:]
	}
	u.mu.Unlock()
	if next != "" {
		info, err := os.Stat(next)
		if err != nil {
			return
		}
		data, err := os.ReadFile(next)
		if err != nil || !looksLikeReplay(data) {
			return
		}
		if u.uploadData(next, info, data, "backfill") {
			u.state.countBackfill()
			_ = u.state.save()
		}
		u.pauseBackfill(u.opts.BackfillEvery)
		return
	}
	u.checkBacklogBatch()
}

// checkBacklogBatch hashes the next batch of older replays and asks the
// site which it already has: those are recorded (and claimed, if linked)
// without uploading; the rest go on the upload list.
func (u *Uploader) checkBacklogBatch() {
	u.mu.Lock()
	n := min(existsBatch, len(u.backlog))
	batch := append([]string{}, u.backlog[:n]...)
	u.mu.Unlock()
	if n == 0 {
		return
	}
	type item struct {
		path string
		info os.FileInfo
		hash string
	}
	var items []item
	var hashes []string
	for _, p := range batch {
		info, err := os.Stat(p)
		if err != nil || info.Size() > maxFileBytes {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil || !looksLikeReplay(data) {
			continue
		}
		h := sha256Hex(data)
		if _, ok := u.state.handled(h); ok {
			u.state.markFile(p, info, h)
			continue
		}
		items = append(items, item{p, info, h})
		hashes = append(hashes, h)
	}
	found := map[string]existing{}
	if len(hashes) > 0 {
		var err error
		found, err = alreadyUploaded(hashes)
		if err != nil {
			log.Printf("backfill: couldn't check with the site (%v); trying again in 10 minutes", err)
			u.pauseBackfill(10 * time.Minute)
			return
		}
	}
	u.mu.Lock()
	u.backlog = u.backlog[n:]
	u.mu.Unlock()
	skipped := 0
	for _, it := range items {
		if ex, ok := found[it.hash]; ok {
			rec := UploadRecord{File: it.path, ReplayID: ex.ID, URL: ex.URL, At: time.Now(), Duplicate: true, Source: "backfill"}
			u.state.record(it.hash, rec)
			u.state.markFile(it.path, it.info, it.hash)
			u.claim(it.hash, rec)
			skipped++
			continue
		}
		u.mu.Lock()
		u.toUpload = append(u.toUpload, it.path)
		u.mu.Unlock()
	}
	_ = u.state.save()
	if len(items) > 0 {
		log.Printf("backfill: checked %d older replay(s): %d already on the site, %d to upload", len(items), skipped, len(items)-skipped)
	}
}

// backfill uploads the n most recent replays from before the start time,
// right away (the `backfill` command).
func (u *Uploader) backfill(n int) int {
	type f struct {
		path string
		mod  time.Time
	}
	var files []f
	dirs, _ := watchRoots(u.explicit)
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !isReplayFile(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil {
				files = append(files, f{filepath.Join(dir, e.Name()), info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > n {
		files = files[:n]
	}
	for _, x := range files {
		u.handle(x.path, "backfill")
	}
	return len(files)
}
