# Commit

A WhatsApp commitment tracker that reads your conversations, extracts promises and obligations, and tracks them to completion.

Commit connects to WhatsApp via the multi-device API, uses Claude to understand your conversations, and builds a live dashboard of who owes what to whom.

## How it works

1. **Link WhatsApp** — scan a QR code to connect your account
2. **AI extracts commitments** — Claude reads new messages and identifies promises, deadlines, and obligations
3. **Auto-resolves** — when a commitment is fulfilled in conversation, Commit marks it done

Everything runs locally on your machine. Messages are decrypted on-device and stored in a local database. Message content is sent to the Claude API for commitment analysis — nothing is stored on a remote server by Commit.

## Features

- **Dashboard** — all open commitments in one view, grouped by chat. Filter by "I owe", "They owe", or "Resolved". Search across everything.
- **Auto-extraction** — Claude reads incoming messages every 10 seconds, identifies promises and obligations, and logs them with the original quote, person, and direction.
- **Auto-resolution** — when a commitment is fulfilled in conversation (a file shared, a task confirmed, someone says "done"), Commit marks it resolved automatically.
- **Follow-ups** — surfaces things others owe you that have gone quiet. Drafts a polite nudge message and lets you send it directly from the dashboard.
- **Reminders** — set a reminder on any commitment. When it's due, Commit sends you a WhatsApp message to your own account so it shows up in your chat list.
- **Favorites** — star important chats or commitments to pin them to a dedicated tab for quick access.
- **Reply from dashboard** — respond to any commitment's chat directly from the Commit UI without switching to WhatsApp.
- **WhatsApp bot commands** — message yourself on WhatsApp to check commitments, search, or mark things done (see commands below).
- **History sync** — when you link a new device, Commit backfills your recent WhatsApp message history so commitments appear immediately.
- **Dark and light theme** — toggle from the dashboard toolbar. Respects your preference across sessions.
- **Mobile web** — the dashboard is fully responsive. Access it from your phone's browser at the same local address.
- **Passcode protection** — the web interface is secured with a passcode. Your Claude API key is encrypted with AES-GCM on disk.

## System requirements

- **macOS** 12 Monterey or later (Apple Silicon or Intel)
- **Windows** 10 or later (64-bit)
- A [Claude API key](https://console.anthropic.com/) (Anthropic account required)
- WhatsApp account with multi-device support

## Install

### Mac (DMG)

Download `Commit-x.x.x.dmg` from Releases, open it, and drag Commit to Applications. Then run:

```bash
/Applications/Commit.app/Contents/MacOS/Commit
```

### Windows

Download `Commit-x.x.x-windows-amd64.zip` from Releases, extract it, and run `Commit.exe`.

### From source

Requires Go 1.22+ and a [Claude API key](https://console.anthropic.com/).

```bash
git clone https://github.com/mitensampat/commit.git
cd commit
go build -o commit .
./commit
```

## Setup

Open [http://commit:9384](http://commit:9384) in your browser (or `localhost:9384`). The setup wizard will walk you through:

1. **Set a passcode** — protects the web interface and encrypts your API key
2. **Enter your Claude API key** — stored locally, encrypted with AES-GCM
3. **Scan the QR code** — links Commit to your WhatsApp account

Once connected, Commit scans incoming messages every 10 seconds and populates the dashboard.

## WhatsApp bot commands

Message yourself on WhatsApp to interact with Commit:

| Command | Description |
|---------|-------------|
| `commitments` | List all open commitments |
| `owe @person` | Show what you owe someone |
| `done <text>` | Mark a commitment as resolved |
| `search <query>` | Find commitments by keyword |
| `help` | Show available commands |

## Architecture

```
main.go              — entry point, wires up components
server/              — HTTP server, API endpoints, auth
server/static/       — embedded web UI (single-page app)
extraction/          — Claude API client, commitment extraction prompt
store/               — SQLite database, message and commitment storage
whatsapp/            — WhatsApp client (whatsmeow), bot command handler
landing/             — marketing landing page
```

Data is stored in `~/.commit/`:
- `commit.db` — SQLite database (messages, commitments, settings)
- WhatsApp session files

## Building releases

```bash
# Mac DMG (ad-hoc signed, for local testing)
./scripts/build-mac.sh

# Mac DMG (signed + notarized, for distribution)
DEVELOPER_ID="Developer ID Application: ..." \
NOTARY_PROFILE="commit-notary" \
./scripts/build-mac.sh

# Windows zip
./scripts/build-windows.sh

# Both platforms
./scripts/build-all.sh
```

## Privacy & data

- All data stays on your machine in `~/.commit/` — messages, commitments, settings, and WhatsApp session files
- Messages are decrypted locally by the WhatsApp multi-device protocol
- Message content (including sender names and timestamps) is sent to the Claude API for commitment extraction — Commit does not filter or redact messages before sending
- Anthropic does not train on API inputs; their [data retention policy](https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching#data-retention-and-privacy) governs what happens on their side
- No cloud storage, no telemetry, no tracking by Commit
- Your WhatsApp linked device session persists until you unlink it from your phone (Settings → Linked Devices) — even if Commit is not running, messages will queue for the next session
- To fully remove Commit: unlink the device from WhatsApp, delete the app, and delete `~/.commit/`

## Third-party services

- **Claude API** (Anthropic) — commitment extraction from message content
- **Formspree** — landing page waitlist form submission (landing page only, not the app)

## License

Private. Not open source.
