# Commit

A WhatsApp commitment tracker that reads your conversations, extracts promises and obligations, and tracks them to completion.

Commit connects to WhatsApp via the multi-device API, uses Claude to understand your conversations, and builds a live dashboard of who owes what to whom.

## How it works

1. **Link WhatsApp** — scan a QR code to connect your account
2. **AI extracts commitments** — Claude reads new messages and identifies promises, deadlines, and obligations
3. **Auto-resolves** — when a commitment is fulfilled in conversation, Commit marks it done

Everything runs locally on your machine. Messages are decrypted on-device. Only small message snippets are sent to the Claude API for analysis — nothing is stored remotely.

## Install

### Mac (DMG)

Download `Commit-x.x.x.dmg` from [Releases](https://github.com/mitensampat/commit/releases), open it, and drag Commit to Applications. Then run:

```bash
/Applications/Commit.app/Contents/MacOS/Commit
```

### Windows

Download `Commit-x.x.x-windows-amd64.zip` from [Releases](https://github.com/mitensampat/commit/releases), extract it, and run `Commit.exe`.

### From source

Requires Go 1.22+ and a [Claude API key](https://console.anthropic.com/).

```bash
git clone https://github.com/mitensampat/commit.git
cd commit
go build -o commit .
./commit
```

## Setup

Open [http://localhost:9384](http://localhost:9384) in your browser. The setup wizard will walk you through:

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

## Privacy

- All data stays on your machine
- Messages are decrypted locally by the WhatsApp multi-device protocol
- Only short message excerpts are sent to the Claude API for analysis
- Claude does not train on API inputs
- No cloud storage, no telemetry, no tracking

## License

Private. Not open source.
