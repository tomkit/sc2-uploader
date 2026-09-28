// sc2-uploader watches StarCraft II's replay folders and uploads each new
// game to StarCraft2.ai, where it gets a replay page and can be coached.
// No account needed; `sc2-uploader link` optionally attributes uploads to
// yours. Installed as a login item by the scripts at
// https://www.starcraft2.ai/uploader.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

// Set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `sc2-uploader %s — auto-upload your StarCraft II replays to StarCraft2.ai

Usage:
  sc2-uploader run              Watch for new replays and upload them (what the installer starts at login).
                                Older replays are backfilled politely in the background: games already
                                on the site are skipped, the rest go up newest first, at most 25 a day.
  sc2-uploader status           Show what's being watched and the latest uploads
  sc2-uploader link             Link uploads to your StarCraft2.ai account (optional)
  sc2-uploader unlink           Remove the account link
  sc2-uploader backfill <n>     Upload your n most recent older replays, once
  sc2-uploader upload <file>    Upload one replay now
  sc2-uploader version

Options:
  --dir <folder>                Watch this folder instead of the default (repeatable)
  --no-backfill                 run: only upload games played from now on

Uploads are public replay pages on StarCraft2.ai, like uploads from the website.
Data: %s
`

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, version, stateDir())
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var dirs multiFlag
	fs.Var(&dirs, "dir", "replay folder to watch (repeatable)")
	noBackfill := fs.Bool("no-backfill", false, "only upload games played from now on")
	_ = fs.Parse(args)

	switch cmd {
	case "run":
		opts := DefaultOptions
		opts.Backfill = !*noBackfill
		os.Exit(runDaemon(dirs, opts))
	case "status":
		os.Exit(status(dirs))
	case "link":
		os.Exit(link())
	case "unlink":
		os.Exit(unlink())
	case "backfill":
		n := 10
		if fs.NArg() > 0 {
			fmt.Sscanf(fs.Arg(0), "%d", &n)
		}
		os.Exit(backfillCmd(dirs, n))
	case "upload":
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: sc2-uploader upload <file.SC2Replay>")
			os.Exit(2)
		}
		os.Exit(uploadOne(fs.Arg(0)))
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Printf(usage, version, stateDir())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Printf(usage, version, stateDir())
		os.Exit(2)
	}
}

func mustState() *State {
	s, err := loadState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "can't read %s: %v\n", statePath(), err)
		os.Exit(1)
	}
	return s
}

// setupLog writes to stderr and to uploader.log in the state dir, starting
// the file over past 1 MB so a long-running daemon never fills the disk.
func setupLog() {
	_ = os.MkdirAll(stateDir(), 0o700)
	p := filepath.Join(stateDir(), "uploader.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 1<<20 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.SetFlags(log.LstdFlags)
}

func runDaemon(dirs []string, opts Options) int {
	hideOwnConsole() // Windows logon task: no window on the desktop
	setupLog()
	// One watcher per user: the login item and a manual `run` must not
	// both upload.
	lock := flock.New(filepath.Join(stateDir(), "run.lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		log.Printf("another sc2-uploader is already running for this user; exiting")
		return 0
	}
	defer lock.Unlock()

	state := mustState()
	if state.Since.IsZero() {
		// First run: the backlog stays put unless `backfill` is asked for.
		state.Since = time.Now()
		if err := state.save(); err != nil {
			log.Printf("can't save state: %v", err)
			return 1
		}
	}
	u, err := newUploader(state, dirs, opts)
	if err != nil {
		log.Printf("can't start watching: %v", err)
		return 1
	}
	log.Printf("sc2-uploader %s → %s (account: %s)", version, serverBase(), linkedText(state))
	if len(u.discover()) == 0 {
		log.Printf("no StarCraft II replay folder found yet; checking again every %s", rescanEvery)
	}
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; close(stop) }()
	u.run(stop)
	log.Printf("stopped")
	return 0
}

func linkedText(s *State) string {
	if s.token() != "" {
		return "linked"
	}
	return "not linked — uploads are anonymous"
}

func status(dirs []string) int {
	s := mustState()
	found, roots := watchRoots(dirs)
	fmt.Printf("sc2-uploader %s\nServer:   %s\nData:     %s\nAccount:  %s\n", version, serverBase(), stateDir(), linkedText(s))
	if !s.Since.IsZero() {
		fmt.Printf("Watching since %s\n", s.Since.Local().Format(time.RFC1123))
	}
	if len(roots) > 0 {
		fmt.Printf("StarCraft II accounts folder(s):\n")
		for _, r := range roots {
			fmt.Printf("  %s\n", r)
		}
	}
	fmt.Printf("Replay folders (%d):\n", len(found))
	for _, d := range found {
		fmt.Printf("  %s\n", d)
	}
	if len(found) == 0 {
		fmt.Println("  none found — is StarCraft II installed, or pass --dir")
	}
	recs := make([]UploadRecord, 0, len(s.Uploads))
	for _, r := range s.Uploads {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].At.After(recs[j].At) })
	fmt.Printf("Uploads: %d\n", len(recs))
	for i, r := range recs {
		if i == 10 {
			break
		}
		line := r.URL
		if r.Rejected != "" {
			line = "rejected: " + r.Rejected
		}
		fmt.Printf("  %s  %s  %s\n", r.At.Local().Format("2006-01-02 15:04"), filepath.Base(r.File), line)
	}
	return 0
}

func link() int {
	s := mustState()
	token, exp, err := linkAccount(os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if old := s.Token; old != "" {
		revoke(old)
	}
	s.Token, s.TokenExpiresAt = token, exp
	if err := s.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("\nLinked. New uploads will show up under your games on StarCraft2.ai.")
	fmt.Println("This link can't spend minerals; it only attributes uploads to you.")
	return 0
}

func unlink() int {
	s := mustState()
	if s.Token == "" {
		fmt.Println("Not linked.")
		return 0
	}
	revoke(s.Token)
	s.Token, s.TokenExpiresAt = "", time.Time{}
	if err := s.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("Unlinked. Uploads are anonymous again.")
	return 0
}

func backfillCmd(dirs []string, n int) int {
	setupLog()
	s := mustState()
	u, err := newUploader(s, dirs, DefaultOptions)
	if err != nil {
		log.Println(err)
		return 1
	}
	if n < 1 || n > 50 {
		fmt.Fprintln(os.Stderr, "backfill takes 1–50 (the site allows 50 uploads a day)")
		return 2
	}
	got := u.backfill(n)
	log.Printf("backfill: looked at %d replay(s)", got)
	return 0
}

func uploadOne(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !looksLikeReplay(data) {
		fmt.Fprintln(os.Stderr, "not a StarCraft II replay")
		return 1
	}
	res, err := upload(path, data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(res.URL)
	if s, err := loadState(); err == nil && s.token() != "" {
		if err := claim(s.token(), res.ID, sha256Hex(data)); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	return 0
}
