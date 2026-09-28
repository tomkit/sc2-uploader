# sc2-uploader: auto-upload your StarCraft II replays to StarCraft2.ai

A small background app for **macOS and Windows**. When a StarCraft II game ends, it uploads the replay to [StarCraft2.ai](https://www.starcraft2.ai), so every game gets a replay page you can review and send to the AI Coach. Free, and no account needed.

**Install and usage: [starcraft2.ai/uploader](https://www.starcraft2.ai/en/uploader)**

```sh
# macOS (Terminal)
curl -fsSL https://www.starcraft2.ai/uploader/install.sh | sh
```
```powershell
# Windows (PowerShell)
powershell -ExecutionPolicy Bypass -c "irm https://www.starcraft2.ai/uploader/install.ps1 | iex"
```

The installers download the binary for your platform from this repo's latest release, check it against `SHA256SUMS`, and register a login item: a LaunchAgent on macOS, a logon Scheduled Task on Windows. They need no admin rights.

## How it works

- **Finds the replay folders**: `Accounts/<account>/<region-toon>/Replays/Multiplayer` for every account. On macOS that's under `~/Library/Application Support/Blizzard/StarCraft II`. On Windows it's under your Documents folder, which the uploader asks Windows for, so OneDrive and redirected folders work. Campaign and challenge replays aren't uploaded.
- **Sleeps until a game ends**: it uses the OS change feed. On macOS that's FSEvents, one stream with no per-file handles. On Windows it's `ReadDirectoryChangesW`. A 5-minute rescan backs it up. A replay is uploaded once its size has stopped changing and it passes the replay-header check.
- **Uploads anonymously** to `POST /api/parse?summary=1`, with local dedup by SHA-256.
- **Catches up politely on older replays**:
  - It asks the site in batches which files it already has (`POST /api/replays/exists`) and skips those without uploading.
  - The rest go up newest first, one every 90 seconds and at most 25 a day. That leaves room in the site's 50-a-day per-IP limit for new games.
  - It pauses while a new game is waiting, and when the site says to slow down (`Retry-After`).
  - Pass `--no-backfill` to `run` to skip older replays entirely.
- **Handles errors**: if a VPN or datacenter network hits the site's bot protection, the uploader says so in the log instead of retrying in a tight loop.

## Commands

```
sc2-uploader run [--dir <folder>] [--no-backfill]   watch and upload (what the login item runs)
sc2-uploader status                                 folders, account link, latest uploads
sc2-uploader link / unlink                          attribute uploads to your StarCraft2.ai account (optional)
sc2-uploader backfill <n>                           upload your n most recent older replays now
sc2-uploader upload <file>                          upload one replay
```

`link` uses a sign-in code you enter at starcraft2.ai/auth/device. The link attributes uploads to you and tells the site which StarCraft II accounts on this computer are yours (the folder names under `Accounts/`). If you're on a [coach plan](https://www.starcraft2.ai/en/coach-plan), each new game you play is then analyzed automatically. Older replays it backfills aren't. The uploader itself can't spend minerals; the plan's monthly gold minerals pay for those analyses.

Data lives in `~/Library/Application Support/sc2-uploader` on macOS and `%LOCALAPPDATA%\sc2-uploader` on Windows: `state.json` (what's been uploaded, and the optional account token, owner-only) and `uploader.log`.

**Uploaded replays are public pages on StarCraft2.ai**, the same as uploading on the website.

## Building

```sh
go test -race ./...
V=v0.2.0; LD="-s -w -X main.version=$V"
CGO_ENABLED=1 GOARCH=arm64 CC="clang -arch arm64"  go build -trimpath -ldflags "$LD" -o dist/sc2-uploader-darwin-arm64 .
CGO_ENABLED=1 GOARCH=amd64 CC="clang -arch x86_64" go build -trimpath -ldflags "$LD" -o dist/sc2-uploader-darwin-amd64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64            go build -trimpath -ldflags "$LD" -o dist/sc2-uploader-windows-amd64.exe .
(cd dist && shasum -a 256 sc2-uploader-* > SHA256SUMS)
```

The macOS builds need cgo for FSEvents. The binaries aren't signed yet, so a browser download may trigger a Gatekeeper or SmartScreen warning. The install commands avoid that and verify the checksum.

## License

MIT
