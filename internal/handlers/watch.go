package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jiotv-go/jiotv_go/v3/pkg/store"
	"github.com/jiotv-go/jiotv_go/v3/pkg/television"
	"github.com/jiotv-go/jiotv_go/v3/pkg/utils"
)

// watchEntry is one row in the local Continue Watching list. The app's
// watchhistory service does not expose an endpoint the server can read, so
// playback pages record their own history instead.
type watchEntry struct {
	Title     string `json:"title"`
	Subtitle  string `json:"subtitle"`
	ImageURL  string `json:"image"`
	URL       string `json:"url"`
	WatchedAt int64  `json:"watchedAt"`
}

const maxWatchEntries = 30

var watchMu sync.Mutex

func watchHistoryPath() string {
	return filepath.Join(store.GetPathPrefix(), "continue_watching.json")
}

func loadWatchEntries() []watchEntry {
	data, err := os.ReadFile(watchHistoryPath())
	if err != nil {
		return nil
	}
	var entries []watchEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return entries
}

// RecordWatch moves the (re)opened item to the top of the watch list,
// deduplicating by URL. Failures are logged and swallowed — history must
// never break playback.
func RecordWatch(title, subtitle, imageURL, playURL string) {
	title = firstNonEmpty(title, subtitle, "Unknown")
	if playURL == "" {
		return
	}

	watchMu.Lock()
	defer watchMu.Unlock()

	entries := loadWatchEntries()
	next := make([]watchEntry, 0, maxWatchEntries)
	next = append(next, watchEntry{
		Title:     title,
		Subtitle:  subtitle,
		ImageURL:  imageURL,
		URL:       playURL,
		WatchedAt: time.Now().Unix(),
	})
	for _, entry := range entries {
		if entry.URL == playURL {
			continue
		}
		next = append(next, entry)
		if len(next) >= maxWatchEntries {
			break
		}
	}

	data, err := json.Marshal(next)
	if err != nil {
		utils.Log.Printf("Continue watching: marshal failed: %v", err)
		return
	}
	if err := os.WriteFile(watchHistoryPath(), data, 0o600); err != nil {
		utils.Log.Printf("Continue watching: write failed: %v", err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// recordChannelWatch resolves a live channel's name/logo before recording.
func recordChannelWatch(channelID, playURL string) {
	channels, err := television.Channels()
	if err == nil {
		for _, channel := range channels.Result {
			if channel.ID == channelID {
				logo := channel.LogoURL
				if logo != "" && !strings.HasPrefix(logo, "http") {
					logo = "https://jiotvimages.cdn.jio.com/dare_images/images/" + logo
				}
				RecordWatch(channel.Name, "Live TV", logo, playURL)
				return
			}
		}
	}
	RecordWatch("Channel "+channelID, "Live TV", "", playURL)
}

// recentWatchItems renders the recorded list as home-rail cards.
func recentWatchItems(limit int) []appRailItemView {
	entries := loadWatchEntries()
	if len(entries) > limit {
		entries = entries[:limit]
	}
	items := make([]appRailItemView, 0, len(entries))
	for _, entry := range entries {
		items = append(items, appRailItemView{
			Title:    entry.Title,
			Subtitle: entry.Subtitle,
			ImageURL: entry.ImageURL,
			URL:      entry.URL,
		})
	}
	return items
}
