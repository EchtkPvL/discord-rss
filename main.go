// discord-rss polls RSS/Atom feeds and posts new items to Discord webhooks.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mmcdole/gofeed"
	"gopkg.in/yaml.v3"
)

type Feed struct {
	Name  string `yaml:"name"`
	URL   string `yaml:"url"`
	Hook  string `yaml:"hook"`
	Image string `yaml:"image"`
}

type Config struct {
	Interval time.Duration `yaml:"interval"`
	Feeds    []Feed        `yaml:"feeds"`
}

// State maps feed URL to the timestamp of the newest item already posted.
type State map[string]time.Time

type embed struct {
	Title       string     `json:"title,omitempty"`
	Description string     `json:"description,omitempty"`
	URL         string     `json:"url,omitempty"`
	Timestamp   string     `json:"timestamp,omitempty"`
	Color       int        `json:"color"`
	Thumbnail   *thumbnail `json:"thumbnail,omitempty"`
}

type thumbnail struct {
	URL string `json:"url"`
}

var (
	configPath = env("CONFIG", "/data/config.yaml")
	statePath  = env("STATE", "/data/state.json")
	httpClient = &http.Client{Timeout: 30 * time.Second}
	tagRe      = regexp.MustCompile(`<[^>]*>`)
	spaceRe    = regexp.MustCompile(`\s+`)
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for {
		interval := run(ctx)
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-time.After(interval):
		}
	}
}

// run performs one polling pass and returns the interval until the next one.
// The config is re-read every pass so edits apply without a restart.
func run(ctx context.Context) time.Duration {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("load config", "err", err)
		return time.Minute
	}
	state, err := loadState()
	if err != nil {
		slog.Error("load state", "err", err)
		return cfg.Interval
	}
	// Never post anything we cannot record, or the same items would be reposted every poll.
	if err := saveState(state); err != nil {
		slog.Error("state not writable, skipping poll (is the data dir owned by uid 65532?)", "err", err)
		return cfg.Interval
	}
	for _, f := range cfg.Feeds {
		if ctx.Err() != nil {
			break
		}
		last, err := poll(ctx, f, state[f.URL])
		if err != nil {
			slog.Error("poll", "feed", f.Name, "err", err)
		}
		if last.Equal(state[f.URL]) {
			continue
		}
		// Save after every feed so a crash cannot cause reposts of finished feeds.
		state[f.URL] = last
		if err := saveState(state); err != nil {
			slog.Error("save state", "err", err)
			break
		}
	}
	return cfg.Interval
}

func loadConfig() (*Config, error) {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Interval: 5 * time.Minute}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	if cfg.Interval < time.Minute {
		cfg.Interval = time.Minute
	}
	for i, f := range cfg.Feeds {
		if f.Name == "" || f.URL == "" || f.Hook == "" {
			return nil, fmt.Errorf("feed #%d: name, url and hook are required", i+1)
		}
	}
	return cfg, nil
}

func loadState() (State, error) {
	s := State{}
	b, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s)
}

func saveState(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath)
}

// poll fetches a feed, posts items newer than last and returns the new high-water mark.
// A feed seen for the first time is only marked, nothing is posted.
func poll(ctx context.Context, f Feed, last time.Time) (time.Time, error) {
	parser := gofeed.NewParser()
	parser.Client = httpClient
	parser.UserAgent = "discord-rss/2"
	feed, err := parser.ParseURLWithContext(f.URL, ctx)
	if err != nil {
		return last, err
	}

	var items []*gofeed.Item
	for _, it := range feed.Items {
		if t := itemTime(it); !t.IsZero() && t.After(last) {
			items = append(items, it)
		}
	}
	if last.IsZero() {
		slog.Info("new feed, marking current position", "feed", f.Name)
		return newest(items, time.Now()), nil
	}
	if len(items) == 0 {
		return last, nil
	}

	// Oldest first so Discord shows them in chronological order.
	slices.SortFunc(items, func(a, b *gofeed.Item) int { return itemTime(a).Compare(itemTime(b)) })

	// Discord allows 10 embeds and 6000 characters across all embeds per message.
	var batch []embed
	var batchLast time.Time
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := sendHook(ctx, f, batch); err != nil {
			return err
		}
		slog.Info("posted", "feed", f.Name, "count", len(batch))
		last, batch, size = batchLast, nil, 0
		return nil
	}
	for _, it := range items {
		e := toEmbed(f, it)
		n := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
		if len(batch) == 10 || size+n > 5500 {
			if err := flush(); err != nil {
				return last, err
			}
		}
		batch = append(batch, e)
		batchLast = itemTime(it)
		size += n
	}
	return last, flush()
}

func newest(items []*gofeed.Item, fallback time.Time) time.Time {
	t := fallback
	for _, it := range items {
		if it := itemTime(it); it.After(t) {
			t = it
		}
	}
	return t
}

func itemTime(it *gofeed.Item) time.Time {
	switch {
	case it.PublishedParsed != nil:
		return *it.PublishedParsed
	case it.UpdatedParsed != nil:
		return *it.UpdatedParsed
	}
	return time.Time{}
}

func toEmbed(f Feed, it *gofeed.Item) embed {
	content := it.Content
	if content == "" {
		content = it.Description
	}
	e := embed{
		Title:       truncate(it.Title, 250),
		Description: truncate(plainText(content), 500),
		URL:         it.Link,
		Timestamp:   itemTime(it).UTC().Format(time.RFC3339),
		Color:       color(content),
	}
	if f.Image != "" {
		e.Thumbnail = &thumbnail{URL: f.Image}
	}
	return e
}

func color(content string) int {
	switch {
	case content == "":
		return 0x000000
	case strings.Contains(strings.ToLower(content), "resolved"):
		return 0x69ffc3
	}
	return 0xffca5f
}

func plainText(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func sendHook(ctx context.Context, f Feed, embeds []embed) error {
	body, err := json.Marshal(map[string]any{
		"username":         f.Name,
		"embeds":           embeds,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.Hook+"?wait=true", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := 5 * time.Second
			if d, err := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); err == nil {
				wait = d
			}
			time.Sleep(wait)
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("webhook: %s: %s", resp.Status, msg)
		}
		return nil
	}
	return errors.New("webhook: rate limited")
}
