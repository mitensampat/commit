# Commit

WhatsApp commitment tracker. Go + whatsmeow + embedded browser UI (server/static/index.html). Menu bar app on macOS, system tray on Windows (fyne.io/systray — macOS builds require CGO).

## Golden Path Build Routine

The full release procedure. Run every step, in order — never publish a build that skipped one.

1. **Bump the version** in ALL six places (they must agree):
   - `server/server.go` — `const AppVersion`
   - `scripts/build-mac.sh` — `VERSION`
   - `scripts/build-windows.sh` — `VERSION`
   - `server/static/index.html` — footer `vX.Y.Z by MS Foundry`
   - `docs/version.json` — powers the in-app update banner
   - `docs/index.html` — comment on line 1, header version badge, `updated <date>` line; add new features to the feature list
2. **Verify**: `go build ./... && go vet ./...` must be clean. Also cross-compile check: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null .`
3. **macOS build** (signed + notarized — NEVER upload ad-hoc or unnotarized builds):
   ```
   DEVELOPER_ID="Developer ID Application: Miten Sampat (694B5W23H7)" \
   NOTARY_PROFILE="alfred-notary" ./scripts/build-mac.sh
   ```
   The notary keychain profile is named `alfred-notary` (created for Alfred; app-specific passwords aren't app-scoped). Notarization takes 1–10 min.
4. **Windows build**: `./scripts/build-windows.sh`
5. **Smoke test the artifacts**:
   - `spctl -a -t open --context context:primary-signature -v Commit-X.Y.Z.dmg` → must say `accepted / source=Notarized Developer ID`
   - Mount the DMG, `codesign --verify --strict` the app, `lipo -archs` shows `x86_64 arm64`, Info.plist version matches
   - `unzip -l` the Windows zip
6. **Commit + push** to `main`. Release notes must use generic names only — never real contact names from the DB.
7. **GitHub release**: `gh release create vX.Y.Z Commit-X.Y.Z.dmg Commit-X.Y.Z-windows-amd64.zip --title ... --notes ...`
8. **Landing page**: the push triggers the "Deploy to GitHub Pages" workflow. Verify it succeeded (`gh run list`) — it fails transiently sometimes; re-run with `gh workflow run "Deploy to GitHub Pages"`. Confirm `curl https://mitensampat.github.io/commit/version.json` shows the new version.
9. **Install locally**: quit the running app, mount the DMG, replace `/Applications/Commit.app`, `open -a Commit`, confirm `curl localhost:9384/api/version`. Always deploy locally via the DMG → /Applications — never leave a loose dev binary as the running instance.

## Architecture notes

- Local deploys of dev builds: replace `/Applications/Commit.app/Contents/MacOS/Commit` or relaunch the app — the user runs the Applications bundle, not `~/.commit/commit`.
- Web sessions persist in SQLite (hashed tokens, 30-day expiry) — restarts don't log the user out.
- The API key is AES-encrypted with a passcode-derived key; the derived key is cached at `~/.commit/.crypto_key` (0600) so restarts can decrypt without a fresh login.
- Snooze (`snoozed_flag=1`) hides via `reminder_at` but never pings; user reminders (`snoozed_flag=0`) ping the self-chat when due.
- `/api/today` ranking lives in `store/today.go` (`RankToday`) and is shared by the Today view and the morning WhatsApp digest — they must never diverge.
- Extraction skips LOW significance entirely; nudge tenets: consequence-driven, one moment per day, own-commitments-only.
- `COMMIT_NO_TRAY=1` runs headless (no menu bar), server blocks on main.
- Icon assets in `assets/` are generated (C-in-rounded-square, Georgia Bold); regenerate with the iconsgen tool if the mark changes, then rebuild `config/AppIcon.icns` via `iconutil`.
