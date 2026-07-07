# feed

A feed reader with a CLI and a web UI. Browse YouTube subscriptions and RSS feeds from the terminal using fzf, or via a local web server with a YouTube-style grid, inline video embeds, and a resizable side panel.

> **Note:** the app currently assumes a single user instance. There is no authentication, and all data is read from and written to local files. Multi-user support is planned.

## Dependencies

### CLI
- `python3` (stdlib only, no pip installs)
- `fzf`
- `xdg-open` (standard on most Linux desktops)

### Web UI (`yt-auth`)
- Go 1.22+

## Installation

Clone the repo and symlink the scripts into `~/.local/bin`:

```bash
git clone <repo-url> ~/src/feed
cd ~/src/feed

ln -s "$PWD/feed"      ~/.local/bin/feed
ln -s "$PWD/rss-fetch" ~/.local/bin/rss-fetch
ln -s "$PWD/yt2rss"    ~/.local/bin/yt2rss
```

Make sure `~/.local/bin` is in your `PATH`.

Build the web UI:

```bash
cd yt-auth && go build -o yt-auth .
```

## Files

| Path | Purpose |
|------|---------|
| `~/.config/feed/rss_urls.txt` | One RSS/Atom URL per line — your feed list |
| `~/.config/feed/yt_credentials.json` | Google OAuth2 credentials (see setup) |
| `~/.config/feed/yt_token.json` | Saved YouTube OAuth token |
| `~/.cache/feed/feeds.tsv` | Fetched feed cache, auto-regenerated after 24h |
| `~/.cache/feed/fetched_urls.txt` | Snapshot of last-synced URLs (incremental refresh) |
| `~/.local/share/feed/seen.txt` | URLs marked as read (shared by CLI and web UI) |

## Setup

### YouTube OAuth (web UI)

1. Go to [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
2. Create an OAuth 2.0 Client ID (type: **Desktop app**)
3. Enable the **YouTube Data API v3**
4. Download the JSON and save it to `~/.config/feed/yt_credentials.json`
5. In the OAuth consent screen, add your Google account as a test user

Then run `./yt-auth/yt-auth` and visit `http://localhost:8080/youtube` to authenticate.

### YouTube subscriptions (CLI / manual)

Export from [Google Takeout](https://takeout.google.com) (YouTube → subscriptions.csv), then:

```bash
tail -n +2 subscriptions.csv | cut -d, -f2 | yt2rss >> ~/.config/feed/rss_urls.txt
```

### Reddit

Paste subreddit feeds directly, or use the web UI's `/reddit` import page:

```bash
echo "https://www.reddit.com/r/linux.rss"        >> ~/.config/feed/rss_urls.txt
echo "https://www.reddit.com/user/username.rss"   >> ~/.config/feed/rss_urls.txt
```

### Any RSS/Atom feed

```bash
echo "https://example.com/feed.xml" >> ~/.config/feed/rss_urls.txt
```

Or use the `/import` page in the web UI to paste URLs or upload an OPML file.

## Usage

### Web UI

```bash
./yt-auth/yt-auth
```

Opens `http://localhost:8080` in your browser. Features:

- Grid layout with thumbnails
- YouTube and Shorts embed inline in a resizable side panel
- Filter by title or channel
- Dismiss items (synced with CLI via `seen.txt`)
- Refresh button with smart incremental fetching

### CLI

```bash
feed
```

fzf opens immediately and items stream in as feeds are fetched. Select an entry and press Enter to open it in the browser. Press Escape to quit.

The cache is reused for 24 hours. To force a refresh:

```bash
rm ~/.cache/feed/feeds.tsv
```

#### Options

```
-u <file>   Use a different rss_urls.txt
-f <file>   Use a different feeds.tsv cache
```

## Tools

### `feed`

The main CLI entry point. Reads from the cache if fresh, otherwise fetches via `rss-fetch`. Smart refresh: only re-fetches URLs added since the last sync; does a full rebuild if URLs were removed.

### `rss-fetch`

Reads RSS/Atom URLs from stdin, fetches them concurrently, and writes tab-separated entries to stdout:

```
date\tchannel\ttitle\turl\tsummary\tthumbnail
```

Errors are written as `# error ...` lines and filtered out by `feed`. Can be used standalone:

```bash
rss-fetch < ~/.config/feed/rss_urls.txt | grep "CGP Grey"
```

### `yt2rss`

Converts YouTube channel URLs (from Google Takeout CSV or `youtube.com/@handle` URLs) to RSS feed URLs. Reads from stdin, writes to stdout:

```bash
echo "https://www.youtube.com/channel/UC2C_jShtL725hvbm1arSV9w" | yt2rss
# https://www.youtube.com/feeds/videos.xml?channel_id=UC2C_jShtL725hvbm1arSV9w

echo "https://www.youtube.com/@CGPGrey" | yt2rss
# fetches the page to resolve the channel ID
```

### `yt-auth`

Go web server providing the feed UI and subscription sync. Runs on `:8080`.

| Route | Purpose |
|-------|---------|
| `/` | Feed reader grid |
| `/youtube` | Connect YouTube and sync subscriptions |
| `/reddit` | Import subreddits from pasted JSON |
| `/import` | Paste RSS URLs or upload OPML |
