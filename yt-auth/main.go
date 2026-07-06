package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
func rssURLsPath() string       { return filepath.Join(configDir(), "rss_urls.txt") }

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

func appendURLs(urls []string) (added int, err error) {
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
  <nav><a href="/youtube">YouTube</a> <a href="/reddit">Reddit</a></nav>
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
			notice = `<p class="success">` + msg + `</p>`
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
			notice = `<p class="success">` + msg + `</p>`
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
			if name := child.Data.DisplayName; name != "" {
				urls = append(urls, "https://www.reddit.com/r/"+name+".rss")
			}
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/youtube", http.StatusSeeOther)
	})

	registerYouTubeHandlers(mux, ytCfg)
	registerRedditHandlers(mux)

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	go func() {
		fmt.Printf("Listening on http://localhost:8080\n")
		openBrowser("http://localhost:8080/youtube")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	_ = srv.Shutdown(context.Background())
}
