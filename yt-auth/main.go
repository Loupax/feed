package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

const (
	listenAddr    = ":8080"
	ytRedirectURL = "http://localhost:8080/youtube/callback"
)

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(home, ".config", "feed")
}

func ytCredentialsPath() string { return filepath.Join(configDir(), "yt_credentials.json") }
func ytTokenPath() string       { return filepath.Join(configDir(), "yt_token.json") }
func rssURLsPath() string        { return filepath.Join(configDir(), "rss_urls.txt") }
func fetchedURLsPath() string   { return filepath.Join(cacheDir(), "fetched_urls.txt") }

func cacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(home, ".cache", "feed")
}

func dataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(home, ".local", "share", "feed")
}

func feedsTSVPath() string { return filepath.Join(cacheDir(), "feeds.tsv") }
func seenPath() string     { return filepath.Join(dataDir(), "seen.txt") }

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, url).Start()
}

func loadTokenFile(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	return &tok, json.Unmarshal(data, &tok)
}

func saveTokenFile(path string, tok *oauth2.Token) error {
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

var urlsMu sync.Mutex

func appendURLs(urls []string) (added int, err error) {
	urlsMu.Lock()
	defer urlsMu.Unlock()
	outputPath := rssURLsPath()
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return 0, err
	}

	existing := map[string]bool{}
	if data, err := os.ReadFile(outputPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if l := strings.TrimSpace(line); l != "" {
				existing[l] = true
			}
		}
	}

	f, err := os.OpenFile(outputPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	for _, u := range urls {
		if !existing[u] {
			fmt.Fprintln(f, u)
			added++
		}
	}
	return added, nil
}

// ---------------------------------------------------------------------------
// HTML
// ---------------------------------------------------------------------------

const pageWrap = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Feed Auth</title>
  <style>
    body { font-family: sans-serif; max-width: 480px; margin: 80px auto; text-align: center; color: #222; }
    h1   { font-size: 1.4rem; margin-bottom: 0.4rem; }
    p    { color: #555; margin-bottom: 2rem; }
    nav  { margin-bottom: 2rem; }
    nav a { margin: 0 0.8rem; color: #555; text-decoration: none; }
    nav a:hover { text-decoration: underline; }
    .btn { display: inline-block; padding: 0.7rem 1.8rem; color: #fff; border: none;
           border-radius: 4px; font-size: 1rem; cursor: pointer; text-decoration: none; }
    .btn-yt     { background: #ff0000; } .btn-yt:hover     { background: #cc0000; }
    .btn-reddit { background: #ff4500; } .btn-reddit:hover { background: #cc3700; }
    .success { color: #2a7a2a; font-weight: bold; margin-top: 1.5rem; }
    .error   { color: #a00;     font-weight: bold; margin-top: 1.5rem; }
  </style>
</head>
<body>
  <nav><a href="/youtube">YouTube</a> <a href="/reddit">Reddit</a> <a href="/import">Import</a></nav>
  %s
</body>
</html>`


func renderPage(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, pageWrap, body)
}

// ---------------------------------------------------------------------------
// YouTube
// ---------------------------------------------------------------------------

func loadYTConfig() (*oauth2.Config, error) {
	data, err := os.ReadFile(ytCredentialsPath())
	if err != nil {
		return nil, fmt.Errorf("credentials not found at %s: %w", ytCredentialsPath(), err)
	}
	cfg, err := google.ConfigFromJSON(data, youtube.YoutubeReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	cfg.RedirectURL = ytRedirectURL
	return cfg, nil
}

func syncYouTube(cfg *oauth2.Config, tok *oauth2.Token) (added, total int, err error) {
	ctx := context.Background()
	ts := cfg.TokenSource(ctx, tok)

	refreshed, err := ts.Token()
	if err != nil {
		return 0, 0, fmt.Errorf("token refresh: %w", err)
	}
	if refreshed.AccessToken != tok.AccessToken {
		_ = saveTokenFile(ytTokenPath(), refreshed)
	}

	svc, err := youtube.NewService(ctx, option.WithHTTPClient(oauth2.NewClient(ctx, ts)))
	if err != nil {
		return 0, 0, fmt.Errorf("create youtube service: %w", err)
	}

	var ids []string
	pageToken := ""
	for {
		call := svc.Subscriptions.List([]string{"snippet"}).
			Mine(true).MaxResults(50).Order("alphabetical")
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return 0, 0, fmt.Errorf("list subscriptions: %w", err)
		}
		for _, item := range resp.Items {
			ids = append(ids, item.Snippet.ResourceId.ChannelId)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	total = len(ids)
	urls := make([]string, total)
	for i, id := range ids {
		urls[i] = "https://www.youtube.com/feeds/videos.xml?channel_id=" + id
	}
	added, err = appendURLs(urls)
	return added, total, err
}

func registerYouTubeHandlers(mux *http.ServeMux, cfg *oauth2.Config) {
	mux.HandleFunc("/youtube", func(w http.ResponseWriter, r *http.Request) {
		msg := r.URL.Query().Get("msg")
		var notice string
		if msg != "" {
			notice = `<p class="success">` + html.EscapeString(msg) + `</p>`
		}

		_, err := loadTokenFile(ytTokenPath())
		if err != nil {
			authURL := cfg.AuthCodeURL("state", oauth2.AccessTypeOffline)
			renderPage(w, fmt.Sprintf(`
			  <h1>YouTube Subscriptions</h1>
			  <p>Sync your YouTube subscriptions to <code>rss_urls.txt</code></p>
			  <a class="btn btn-yt" href="%s">Connect YouTube</a>%s`, authURL, notice))
			return
		}
		renderPage(w, fmt.Sprintf(`
		  <h1>YouTube Subscriptions</h1>
		  <p>Sync your YouTube subscriptions to <code>rss_urls.txt</code></p>
		  <form method="POST" action="/youtube/sync">
		    <button class="btn btn-yt" type="submit">Sync Subscriptions</button>
		  </form>%s`, notice))
	})

	mux.HandleFunc("/youtube/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/youtube", http.StatusSeeOther)
			return
		}
		tok, err := loadTokenFile(ytTokenPath())
		if err != nil {
			http.Redirect(w, r, "/youtube", http.StatusSeeOther)
			return
		}
		added, total, err := syncYouTube(cfg, tok)
		if err != nil {
			renderPage(w, fmt.Sprintf(`<p class="error">Sync failed: %s</p>
			  <form method="POST" action="/youtube/sync"><button class="btn btn-yt" type="submit">Retry</button></form>`, err))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/youtube?msg=Synced+%d+subscriptions+(%d+new)", total, added), http.StatusSeeOther)
	})

	mux.HandleFunc("/youtube/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			renderPage(w, `<p class="error">Authentication failed: no code received.</p>
			  <a class="btn btn-yt" href="/youtube">Try again</a>`)
			return
		}
		tok, err := cfg.Exchange(context.Background(), code)
		if err != nil {
			renderPage(w, fmt.Sprintf(`<p class="error">Token exchange failed: %s</p>
			  <a class="btn btn-yt" href="/youtube">Try again</a>`, err))
			return
		}
		_ = saveTokenFile(ytTokenPath(), tok)

		added, total, err := syncYouTube(cfg, tok)
		if err != nil {
			renderPage(w, fmt.Sprintf(`<p class="error">Authenticated, but sync failed: %s</p>
			  <form method="POST" action="/youtube/sync"><button class="btn btn-yt" type="submit">Retry sync</button></form>`, err))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/youtube?msg=Synced+%d+subscriptions+(%d+new)", total, added), http.StatusSeeOther)
	})
}

// ---------------------------------------------------------------------------
// Reddit
// ---------------------------------------------------------------------------

func registerRedditHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/reddit", func(w http.ResponseWriter, r *http.Request) {
		msg := r.URL.Query().Get("msg")
		var notice string
		if msg != "" {
			notice = `<p class="success">` + html.EscapeString(msg) + `</p>`
		}
		renderPage(w, fmt.Sprintf(`
		  <h1>Reddit Subscriptions</h1>
		  <p>
		    Open <a href="https://www.reddit.com/subreddits/mine.json?limit=100" target="_blank">
		    subreddits/mine.json</a> while logged in to Reddit, then paste the JSON below.
		  </p>
		  <form method="POST" action="/reddit/import">
		    <textarea name="json" rows="8"
		      style="width:100%%;box-sizing:border-box;font-size:0.8rem;margin-bottom:1rem"
		      placeholder="Paste JSON here..."></textarea>
		    <br>
		    <button class="btn btn-reddit" type="submit">Import</button>
		  </form>%s`, notice))
	})

	mux.HandleFunc("/reddit/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/reddit", http.StatusSeeOther)
			return
		}

		var result struct {
			Data struct {
				After    string `json:"after"`
				Children []struct {
					Data struct {
						DisplayName string `json:"display_name"`
					} `json:"data"`
				} `json:"children"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("json")), &result); err != nil {
			renderPage(w, fmt.Sprintf(`<p class="error">Invalid JSON: %s</p>
			  <a class="btn btn-reddit" href="/reddit">Try again</a>`, err))
			return
		}

		var urls []string
		for _, child := range result.Data.Children {
			name := child.Data.DisplayName
			if name == "" || strings.ContainsAny(name, "\r\n") {
				continue
			}
			urls = append(urls, "https://www.reddit.com/r/"+name+".rss")
		}
		if len(urls) == 0 {
			renderPage(w, `<p class="error">No subreddits found in pasted JSON.</p>
			  <a class="btn btn-reddit" href="/reddit">Try again</a>`)
			return
		}

		added, err := appendURLs(urls)
		if err != nil {
			renderPage(w, fmt.Sprintf(`<p class="error">Failed to save: %s</p>
			  <a class="btn btn-reddit" href="/reddit">Try again</a>`, err))
			return
		}

		msg := fmt.Sprintf("Imported %d subreddits (%d new)", len(urls), added)
		if result.Data.After != "" {
			msg += " — more pages available, paste the next batch"
		}
		http.Redirect(w, r, "/reddit?msg="+strings.ReplaceAll(msg, " ", "+"), http.StatusSeeOther)
	})
}

// ---------------------------------------------------------------------------
// Feed reader
// ---------------------------------------------------------------------------

type FeedItem struct {
	Date      string `json:"date"`
	Channel   string `json:"channel"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Summary   string `json:"summary"`
	Thumbnail string `json:"thumbnail,omitempty"`
}

func readFeeds() ([]FeedItem, error) {
	data, err := os.ReadFile(feedsTSVPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	seen := map[string]bool{}
	if sd, err := os.ReadFile(seenPath()); err == nil {
		for _, line := range strings.Split(string(sd), "\n") {
			if u := strings.TrimSpace(line); u != "" {
				seen[u] = true
			}
		}
	}

	var items []FeedItem
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 4 {
			continue
		}
		if seen[parts[3]] {
			continue
		}
		summary, thumb := "", ""
		if len(parts) >= 5 {
			summary = strings.ReplaceAll(parts[4], `\n`, "\n")
		}
		if len(parts) == 6 {
			thumb = parts[5]
		}
		items = append(items, FeedItem{
			Date:      parts[0],
			Channel:   parts[1],
			Title:     parts[2],
			URL:       parts[3],
			Summary:   summary,
			Thumbnail: thumb,
		})
	}
	return items, nil
}

var refreshMu sync.Mutex

func findRSSFetch() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	for _, candidate := range []string{
		filepath.Join(filepath.Dir(exe), "rss-fetch"),
		filepath.Join(filepath.Dir(exe), "..", "rss-fetch"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("rss-fetch not found near %s", exe)
}

func readURLLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return lines
}

func execRSSFetch(urls []string, appendMode bool) error {
	rssFetch, err := findRSSFetch()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir(), 0755); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	out, err := os.OpenFile(feedsTSVPath(), flags, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := exec.Command(rssFetch)
	cmd.Stdin = strings.NewReader(strings.Join(urls, "\n"))
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runRefresh() error {
	current := readURLLines(rssURLsPath())
	if len(current) == 0 {
		return fmt.Errorf("no URLs in %s", rssURLsPath())
	}

	fetched := readURLLines(fetchedURLsPath())
	fetchedSet := make(map[string]bool, len(fetched))
	for _, u := range fetched {
		fetchedSet[u] = true
	}
	currentSet := make(map[string]bool, len(current))
	for _, u := range current {
		currentSet[u] = true
	}

	// Detect removed URLs
	removed := false
	for _, u := range fetched {
		if !currentSet[u] {
			removed = true
			break
		}
	}

	// Collect added URLs
	var added []string
	for _, u := range current {
		if !fetchedSet[u] {
			added = append(added, u)
		}
	}

	if len(fetched) == 0 || removed {
		// Full refresh: URLs were removed (or first run)
		if err := execRSSFetch(current, false); err != nil {
			return err
		}
	} else if len(added) > 0 {
		// Incremental: only fetch newly added URLs
		if err := execRSSFetch(added, true); err != nil {
			return err
		}
	}
	// else: nothing changed, no-op

	// Update snapshot
	return os.WriteFile(fetchedURLsPath(), []byte(strings.Join(current, "\n")+"\n"), 0644)
}

const feedPageHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Feed</title>
  <style>
    *{box-sizing:border-box;margin:0;padding:0}
    body{font-family:sans-serif;background:#f5f5f5;color:#222}
    header{
      position:sticky;top:0;z-index:10;background:#fff;
      border-bottom:1px solid #ddd;padding:.6rem 1rem;
      display:flex;align-items:center;gap:.75rem
    }
    nav a{color:#555;text-decoration:none;font-size:.9rem}
    nav a:hover{text-decoration:underline}
    #q{
      flex:1;padding:.35rem .7rem;border:1px solid #ccc;
      border-radius:4px;font-size:.95rem
    }
    #status{font-size:.82rem;color:#999;white-space:nowrap}
    #refresh{
      padding:.35rem .9rem;background:#333;color:#fff;
      border:none;border-radius:4px;cursor:pointer;font-size:.88rem
    }
    #refresh:hover{background:#111}
    #refresh:disabled{background:#aaa;cursor:default}
    ul{list-style:none;display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:1rem;padding:0 1rem 1rem;margin-top:.75rem}
    li{background:#fff;border:1px solid #e0e0e0;border-radius:8px;overflow:hidden;cursor:pointer;transition:box-shadow .15s}
    li:hover{box-shadow:0 2px 10px rgba(0,0,0,.12)}
    .thumb-wrap{position:relative;aspect-ratio:16/9;background:#e8e8e8;overflow:hidden}
    .thumb{width:100%;height:100%;object-fit:cover;display:block}
    .meta{padding:.55rem .7rem .7rem}
    .ttl a{
      font-size:.9rem;font-weight:600;color:#0f0f0f;text-decoration:none;
      display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden
    }
    .ttl a:hover{color:#00c}
    .ttl a.seen{color:#bbb}
    .byline{display:flex;gap:.4rem;font-size:.75rem;color:#aaa;margin-top:.3rem;align-items:center}
    .ch{color:#c00;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:55%}
    .dismiss{
      position:absolute;top:6px;right:6px;background:rgba(0,0,0,.6);border:none;
      color:#fff;font-size:.9rem;cursor:pointer;width:24px;height:24px;border-radius:50%;
      display:flex;align-items:center;justify-content:center;
      opacity:0;transition:opacity .15s;flex-shrink:0
    }
    li:hover .dismiss{opacity:1}
    .dismiss:hover{background:rgba(180,0,0,.85)}
    li.active{outline:2px solid #c00;outline-offset:-2px}
    #empty{text-align:center;color:#aaa;margin-top:5rem;font-size:.95rem}
    #panel{
      position:fixed;top:0;right:0;bottom:0;width:900px;max-width:100vw;
      background:#fff;border-left:1px solid #ddd;
      display:flex;flex-direction:column;
      transform:translateX(100%);transition:transform .25s ease;z-index:100
    }
    #panel-handle{
      position:absolute;left:0;top:0;bottom:0;width:6px;
      cursor:ew-resize;z-index:1;flex-shrink:0
    }
    #panel-handle::after{
      content:'';position:absolute;left:2px;top:50%;transform:translateY(-50%);
      width:2px;height:40px;border-radius:2px;background:#ddd;transition:background .15s
    }
    #panel-handle:hover::after,#panel.resizing #panel-handle::after{background:#aaa}
    #panel.open{transform:none}
    #panel-header{
      display:flex;align-items:center;gap:.6rem;
      padding:.6rem .75rem;border-bottom:1px solid #eee;flex-shrink:0
    }
    #panel-close{
      background:none;border:none;font-size:1.3rem;cursor:pointer;
      color:#666;line-height:1;padding:.1rem .3rem;margin-left:auto
    }
    #panel-close:hover{color:#000}
    #panel-ch{font-size:.8rem;font-weight:600;color:#c00}
    #panel-date{font-size:.75rem;color:#aaa}
    #panel-body{flex:1;overflow-y:auto;padding:.75rem}
    #panel-title{font-size:1rem;font-weight:600;margin-bottom:.75rem;line-height:1.4}
    #panel-title a{color:#0f0f0f;text-decoration:none}
    #panel-title a:hover{text-decoration:underline}
    #panel-embed{margin-bottom:.75rem}
    #panel-embed iframe{width:100%;aspect-ratio:16/9;border:none;border-radius:4px}
    #panel-summary{font-size:.85rem;color:#444;white-space:pre-wrap;line-height:1.6}
    #overlay{
      display:none;position:fixed;inset:0;background:rgba(0,0,0,.25);z-index:99
    }
    #overlay.open{display:block}
  </style>
</head>
<body>
<header>
  <nav><a href="/youtube">YouTube</a> &nbsp; <a href="/reddit">Reddit</a> &nbsp; <a href="/import">Import</a></nav>
  <input id="q" type="search" placeholder="Filter…" autofocus>
  <span id="status"></span>
  <button id="refresh">Refresh</button>
</header>
<ul id="list"></ul>
<div id="empty" hidden>No items.</div>
<div id="overlay"></div>
<div id="panel">
  <div id="panel-handle"></div>
  <div id="panel-header">
    <span id="panel-ch"></span>
    <span id="panel-date"></span>
    <button id="panel-close">×</button>
  </div>
  <div id="panel-body">
    <div id="panel-title"></div>
    <div id="panel-embed"></div>
    <div id="panel-summary"></div>
  </div>
</div>
<script>
const list=document.getElementById('list');
const status=document.getElementById('status');
const empty=document.getElementById('empty');
const q=document.getElementById('q');
const btn=document.getElementById('refresh');
const panel=document.getElementById('panel');
const overlay=document.getElementById('overlay');
const panelCh=document.getElementById('panel-ch');
const panelDate=document.getElementById('panel-date');
const panelTitle=document.getElementById('panel-title');
const panelEmbed=document.getElementById('panel-embed');
const panelSummary=document.getElementById('panel-summary');

let activeNode=null;

function openPanel(it, li){
  const vid=ytVideoId(it.url);

  panelCh.textContent=it.channel;
  panelDate.textContent=it.date;
  panelTitle.innerHTML='<a href="'+esc(it.url)+'" target="_blank" rel="noopener">'+esc(it.title)+'</a>';
  panelEmbed.innerHTML=vid
    ?'<iframe src="https://www.youtube.com/embed/'+vid+'?autoplay=1" allowfullscreen allow="autoplay"></iframe>'
    :'';
  panelSummary.textContent=it.summary||'';

  if(activeNode) activeNode.classList.remove('active');
  activeNode=li;
  li.classList.add('active');

  panel.classList.add('open');
  overlay.classList.add('open');
}

function closePanel(){
  panel.classList.remove('open');
  overlay.classList.remove('open');
  panelEmbed.innerHTML=''; // stop video
  if(activeNode){ activeNode.classList.remove('active'); activeNode=null; }
}

document.getElementById('panel-close').addEventListener('click', closePanel);
overlay.addEventListener('click', closePanel);
document.addEventListener('keydown', e=>{ if(e.key==='Escape') closePanel(); });

// Resizable panel
const PANEL_MIN=280, PANEL_MAX=900;
let panelW=Math.min(PANEL_MAX, Math.max(PANEL_MIN, parseInt(localStorage.getItem('panelW'))||PANEL_MAX));

function applyPanelW(w){
  panelW=Math.max(PANEL_MIN, Math.min(PANEL_MAX, Math.round(w)));
  panel.style.width=panelW+'px';
  localStorage.setItem('panelW', panelW);
}
applyPanelW(panelW);

(()=>{
  const handle=document.getElementById('panel-handle');
  let dragging=false;

  function startDrag(clientX){
    dragging=true;
    panel.classList.add('resizing');
    document.body.style.cssText+='user-select:none;cursor:ew-resize';
  }
  function moveDrag(clientX){
    if(!dragging) return;
    applyPanelW(window.innerWidth-clientX);
  }
  function endDrag(){
    if(!dragging) return;
    dragging=false;
    panel.classList.remove('resizing');
    document.body.style.userSelect='';
    document.body.style.cursor='';
  }

  handle.addEventListener('mousedown', e=>{ e.preventDefault(); startDrag(e.clientX); });
  window.addEventListener('mousemove', e=>moveDrag(e.clientX));
  window.addEventListener('mouseup', endDrag);

  handle.addEventListener('touchstart', e=>{ e.preventDefault(); startDrag(e.touches[0].clientX); },{passive:false});
  window.addEventListener('touchmove', e=>moveDrag(e.touches[0].clientX),{passive:true});
  window.addEventListener('touchend', endDrag);
})();

const BUFFER=3;    // extra grid rows above/below viewport
const META_H=75;   // px of card below the thumbnail (title + channel + padding)
const GAP=16;      // matches gap:1rem in CSS

let COLS=4, ROW_H=220;  // recalculated by updateLayout()
let all=[], visible=[], lastS=-1, lastE=-1, rafId=null;
const nodes=new Map(); // index -> <li>

function updateLayout(){
  const w=list.offsetWidth||window.innerWidth;
  COLS=Math.max(1,Math.floor((w+GAP)/(280+GAP)));
  const cardW=(w-(COLS-1)*GAP)/COLS;
  ROW_H=cardW*(9/16)+META_H+GAP;
}

function esc(s){
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;')
                  .replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function ytVideoId(url){
  const m=url.match(/[?&]v=([A-Za-z0-9_-]{11})/)||url.match(/\/shorts\/([A-Za-z0-9_-]{11})/);
  return m?m[1]:null;
}

function markSeen(url){
  fetch('/api/seen',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({url})});
}

function makeItem(it){
  const li=document.createElement('li');
  const vid=ytVideoId(it.url);
  li.innerHTML=
    '<div class="thumb-wrap">'+
      (it.thumbnail?'<img class="thumb" src="'+esc(it.thumbnail)+'" loading="lazy" alt="">':'')+
      '<button class="dismiss" title="Dismiss">×</button>'+
    '</div>'+
    '<div class="meta">'+
      '<div class="ttl"><a href="'+esc(it.url)+'" target="_blank" rel="noopener">'+esc(it.title)+'</a></div>'+
      '<div class="byline">'+
        '<span class="ch">'+esc(it.channel)+'</span>'+
        '<span class="date">'+esc(it.date)+'</span>'+
      '</div>'+
    '</div>'+
    '';
  li.querySelector('a').addEventListener('click', e=>{
    e.stopPropagation();
    li.querySelector('a').classList.add('seen');
    markSeen(it.url);
  });
  li.querySelector('.dismiss').addEventListener('click', e=>{
    e.stopPropagation();
    markSeen(it.url);
    all=all.filter(x=>x.url!==it.url);
    visible=visible.filter(x=>x.url!==it.url);
    if(activeNode===li) closePanel();
    reset();
  });
  li.addEventListener('click', e=>{
    if(e.target.classList.contains('dismiss')) return;
    openPanel(it, li);
    li.querySelector('a').classList.add('seen');
    markSeen(it.url);
  });
  return li;
}

function paint(){
  updateLayout();
  const totalRows=Math.ceil(visible.length/COLS);
  const listTop=list.getBoundingClientRect().top;
  const scrolled=Math.max(0,-listTop);
  const startRow=Math.max(0, Math.floor(scrolled/ROW_H)-BUFFER);
  const endRow=Math.min(totalRows, Math.ceil((scrolled+window.innerHeight)/ROW_H)+BUFFER);
  const s=startRow*COLS;
  const e=Math.min(visible.length, endRow*COLS);
  if(s===lastS && e===lastE) return;

  // Remove nodes leaving from the top
  for(let i=lastS; i<Math.min(s,lastE); i++){
    nodes.get(i)?.remove(); nodes.delete(i);
  }
  // Remove nodes leaving from the bottom
  for(let i=Math.max(e,lastS); i<lastE; i++){
    nodes.get(i)?.remove(); nodes.delete(i);
  }
  // Prepend nodes entering from the top (iterate high→low so each goes before the previous)
  const topLimit=Math.min(lastS<0?e:lastS, e);
  for(let i=topLimit-1; i>=s; i--){
    const node=makeItem(visible[i]);
    nodes.set(i,node);
    list.insertBefore(node, list.firstChild);
  }
  // Append nodes entering from the bottom
  const botStart=lastS<0?s:Math.max(lastE,s);
  for(let i=botStart; i<e; i++){
    if(nodes.has(i)) continue;
    const node=makeItem(visible[i]);
    nodes.set(i,node);
    list.appendChild(node);
  }

  list.style.paddingTop=(startRow*ROW_H)+'px';
  list.style.paddingBottom=Math.max(0,(totalRows-endRow)*ROW_H)+'px';
  lastS=s; lastE=e;
  status.textContent=visible.length+' items';
  empty.hidden=visible.length>0;
}

function reset(){
  list.innerHTML=''; nodes.clear(); lastS=-1; lastE=-1;
  window.scrollTo(0,0);
  paint();
}

function schedule(){
  if(!rafId) rafId=requestAnimationFrame(()=>{rafId=null; paint();});
}

function applyFilter(){
  const qv=q.value.toLowerCase();
  visible=qv
    ? all.filter(it=>it.title.toLowerCase().includes(qv)||it.channel.toLowerCase().includes(qv))
    : all.slice();
  reset();
}

async function load(){
  status.textContent='Loading…';
  const r=await fetch('/api/feeds');
  all=await r.json()||[];
  visible=all.slice();
  paint();
}

async function doRefresh(){
  btn.disabled=true; btn.textContent='Refreshing…';
  try{
    const r=await fetch('/api/refresh',{method:'POST'});
    if(!r.ok) throw new Error(await r.text());
    all=await r.json()||[];
    visible=all.slice();
    reset();
  }catch(e){status.textContent='Error: '+e.message;}
  finally{btn.disabled=false; btn.textContent='Refresh';}
}

window.addEventListener('scroll', schedule, {passive:true});
q.addEventListener('input', applyFilter);
btn.addEventListener('click', doRefresh);

let resizeTimer=null;
window.addEventListener('resize',()=>{
  clearTimeout(resizeTimer);
  resizeTimer=setTimeout(()=>{list.innerHTML='';nodes.clear();lastS=-1;lastE=-1;paint();},120);
},{passive:true});

load();
</script>
</body>
</html>`

func registerFeedHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, feedPageHTML)
	})

	mux.HandleFunc("/api/feeds", func(w http.ResponseWriter, r *http.Request) {
		items, err := readFeeds()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if items == nil {
			items = []FeedItem{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	})

	mux.HandleFunc("/api/seen", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(dataDir(), 0755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f, err := os.OpenFile(seenPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		fmt.Fprintln(f, body.URL)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		refreshMu.Lock()
		defer refreshMu.Unlock()

		if err := runRefresh(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		items, err := readFeeds()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if items == nil {
			items = []FeedItem{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	})
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

type opmlOutline struct {
	XMLUrl   string        `xml:"xmlUrl,attr"`
	Outlines []opmlOutline `xml:"outline"`
}

type opmlDoc struct {
	Outlines []opmlOutline `xml:"body>outline"`
}

func parseOPML(data []byte) ([]string, error) {
	var doc opmlDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var urls []string
	var walk func([]opmlOutline)
	walk = func(outlines []opmlOutline) {
		for _, o := range outlines {
			if o.XMLUrl != "" {
				urls = append(urls, o.XMLUrl)
			}
			walk(o.Outlines)
		}
	}
	walk(doc.Outlines)
	return urls, nil
}

func registerImportHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			msg := r.URL.Query().Get("msg")
			var notice string
			if msg != "" {
				notice = `<p class="success">` + html.EscapeString(msg) + `</p>`
			}
			renderPage(w, fmt.Sprintf(`
			  <h1>Import Feeds</h1>
			  <p>Paste RSS/Atom URLs (one per line) and/or upload an OPML file.
			     New URLs are appended to <code>rss_urls.txt</code>.</p>
			  <form method="POST" action="/import" enctype="multipart/form-data">
			    <textarea name="urls" rows="10"
			      style="width:100%%;box-sizing:border-box;font-size:0.85rem;margin-bottom:1rem;font-family:monospace"
			      placeholder="https://example.com/feed.xml&#10;https://www.youtube.com/feeds/videos.xml?channel_id=..."></textarea>
			    <div style="margin-bottom:1.2rem">
			      <label style="font-size:.9rem;color:#555">OPML file: </label>
			      <input type="file" name="opml" accept=".opml,.xml">
			    </div>
			    <button class="btn" style="background:#333" type="submit">Import</button>
			  </form>%s`, notice))
			return
		}

		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "form parse error: "+err.Error(), http.StatusBadRequest)
			return
		}

		var urls []string

		// Plain-text URLs from textarea
		for _, line := range strings.Split(r.FormValue("urls"), "\n") {
			if u := strings.TrimSpace(line); u != "" && !strings.HasPrefix(u, "#") {
				urls = append(urls, u)
			}
		}

		// OPML file
		if file, _, err := r.FormFile("opml"); err == nil {
			defer file.Close()
			data := make([]byte, 10<<20)
			n, _ := file.Read(data)
			opmlURLs, err := parseOPML(data[:n])
			if err != nil {
				renderPage(w, fmt.Sprintf(`<p class="error">OPML parse error: %s</p>
				  <a class="btn" style="background:#333" href="/import">Try again</a>`, err))
				return
			}
			urls = append(urls, opmlURLs...)
		}

		if len(urls) == 0 {
			renderPage(w, `<p class="error">No URLs found.</p>
			  <a class="btn" style="background:#333" href="/import">Back</a>`)
			return
		}

		added, err := appendURLs(urls)
		if err != nil {
			renderPage(w, fmt.Sprintf(`<p class="error">Failed to save: %s</p>
			  <a class="btn" style="background:#333" href="/import">Try again</a>`, err))
			return
		}
		http.Redirect(w, r,
			fmt.Sprintf("/import?msg=Added+%d+URLs+(%d+new,+%d+already+present)",
				len(urls), added, len(urls)-added),
			http.StatusSeeOther)
	})
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	ytCfg, err := loadYTConfig()
	if err != nil {
		log.Fatalf(
			"YouTube credentials error: %v\n\n"+
				"Download OAuth 2.0 credentials from Google Cloud Console:\n"+
				"  1. Visit https://console.cloud.google.com/apis/credentials\n"+
				"  2. Create an OAuth 2.0 Client ID (type: Desktop app)\n"+
				"  3. Enable the YouTube Data API v3\n"+
				"  4. Download JSON → save to: %s\n",
			err, ytCredentialsPath(),
		)
	}

	mux := http.NewServeMux()

	registerFeedHandlers(mux)
	registerYouTubeHandlers(mux, ytCfg)
	registerRedditHandlers(mux)
	registerImportHandlers(mux)

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: mux}

	go func() {
		fmt.Printf("Listening on http://localhost:8080\n")
		openBrowser("http://localhost:8080")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	_ = srv.Shutdown(context.Background())
}
