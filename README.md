# feed

A minimal CLI feed reader. Browse YouTube subscriptions and RSS feeds from the terminal using fzf, with a live preview pane for summaries.

## Dependencies

- `python3` (stdlib only, no pip installs)
- `fzf`
- `xdg-open` (standard on most Linux desktops)

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

## Files

| Path | Purpose |
|------|---------|
| `~/.config/feed/rss_urls.txt` | One RSS/Atom URL per line — your feed list |
| `~/.cache/feed/feeds.tsv` | Fetched feed cache, auto-regenerated after 24h |

## Setup

### YouTube subscriptions

Export your subscriptions from [Google Takeout](https://takeout.google.com) (select YouTube → subscriptions.csv), then convert to RSS URLs:

```bash
tail -n +2 subscriptions.csv | cut -d, -f2 | yt2rss >> ~/.config/feed/rss_urls.txt
```

### Reddit

Append subreddit or user feeds directly:

```bash
echo "https://www.reddit.com/r/linux.rss"        >> ~/.config/feed/rss_urls.txt
echo "https://www.reddit.com/user/username.rss"   >> ~/.config/feed/rss_urls.txt
```

### Any RSS/Atom feed

```bash
echo "https://example.com/feed.xml" >> ~/.config/feed/rss_urls.txt
```

## Usage

```bash
feed
```

fzf opens immediately and items stream in as feeds are fetched. Select an entry and press Enter to open it in the browser. Press Escape to quit without opening anything.

The cache is reused for 24 hours. To force a refresh, delete the cache:

```bash
rm ~/.cache/feed/feeds.tsv
```

### Options

```
-u <file>   Use a different rss_urls.txt
-f <file>   Use a different feeds.tsv cache
```

```bash
feed -u ~/work/rss_urls.txt -f /tmp/work_feeds.tsv
```

## Tools

### `feed`

The main entry point. Reads from the cache if fresh, otherwise fetches via `rss-fetch`. Presents results in fzf with a summary preview pane on the right.

### `rss-fetch`

Reads RSS/Atom URLs from stdin, fetches them concurrently, and writes tab-separated entries to stdout:

```
date\tchannel\ttitle\turl\tsummary
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
