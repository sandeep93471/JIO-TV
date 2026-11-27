package handlers

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jiotv-go/jiotv_go/v3/pkg/television"
	"github.com/jiotv-go/jiotv_go/v3/pkg/utils"
)

// premiumSearchResultView pairs a premium catalog match with its ready-to-use
// play and detail links so templates do not rebuild query strings.
type premiumSearchResultView struct {
	ProviderID   string
	ProviderName string
	Item         television.PremiumProviderCatalogItem
	PlayURL      string
	DetailURL    string
}

// premiumItemPlayURL builds the /premium/providers/{id}/play URL for a catalog
// item from its playback fields.
func premiumItemPlayURL(providerID string, item television.PremiumProviderCatalogItem) string {
	playURL := "/premium/providers/" + url.PathEscape(providerID) + "/play?streamType=" + url.QueryEscape(item.StreamType)
	if item.ChannelID != "" {
		playURL += "&channelId=" + url.QueryEscape(item.ChannelID)
	}
	if item.ContentID != "" {
		playURL += "&contentId=" + url.QueryEscape(item.ContentID)
	}
	if item.SubCategoryID != "" {
		playURL += "&subCategoryId=" + url.QueryEscape(item.SubCategoryID)
	}
	return playURL
}

// premiumItemDetailURL builds the detail page URL for a catalog item.
func premiumItemDetailURL(providerID, itemID string) string {
	return "/premium/providers/" + url.PathEscape(providerID) + "/item/" + url.PathEscape(itemID)
}

func premiumResultView(match television.PremiumSearchResult) premiumSearchResultView {
	return premiumSearchResultView{
		ProviderID:   match.ProviderID,
		ProviderName: match.ProviderName,
		Item:         match.Item,
		PlayURL:      premiumItemPlayURL(match.ProviderID, match.Item),
		DetailURL:    premiumItemDetailURL(match.ProviderID, match.Item.ID),
	}
}

// appSearchGroupView is one app-search result tab rendered as a section on
// the search page (Live, Catchup, Channels, Videos...).
type appSearchGroupView struct {
	Name  string
	Items []appRailItemView
}

// SearchHandler renders /search?q=<query> with the app's own search results
// (live, catchup, channels, clips) plus matching premium provider catalog
// items from the server-side index. The optional p=<providerID> parameter
// scopes the search to one OTT provider — used by the per-provider search
// box, which mirrors the app's in-app catalog filtering.
func SearchHandler(c *fiber.Ctx) error {
	query := strings.TrimSpace(c.Query("q"))
	providerScope := strings.TrimSpace(c.Query("p"))
	now := time.Now()

	premiumResults := make([]premiumSearchResultView, 0)
	appGroups := make([]appSearchGroupView, 0)
	if query != "" {
		for _, match := range television.SearchPremiumProviders(query) {
			if providerScope != "" && !strings.EqualFold(match.ProviderID, providerScope) {
				continue
			}
			premiumResults = append(premiumResults, premiumResultView(match))
		}

		// A scoped search is browsing one provider's catalog — live/catchup
		// channels are noise there and are skipped.
		if providerScope == "" {
			if appResults, err := television.AppSearch(query); err != nil {
				utils.Log.Printf("App search failed for %q: %v", query, err)
			} else if appResults != nil {
				addGroup := func(name, railType string, items []television.AppRailItem) {
					if len(items) == 0 {
						return
					}
					rail := television.AppRail{Type: railType, Items: items}
					appGroups = append(appGroups, appSearchGroupView{Name: name, Items: appRailItems(rail, now)})
				}
				data := appResults.Data
				addGroup("Live", "live", data.Live)
				addGroup("Catchup", "content", data.Catchup)
				addGroup("Channels", "channel", data.Channels)
				addGroup("Videos", "content", data.Videos)
				addGroup("Movies", "content", data.Movies)
				addGroup("Shows", "content", data.Shows)
			}
		}
	}

	var channelResults interface{}
	if providerScope == "" {
		channelResults = television.SearchChannels(query)
	}

	return c.Render("views/search_results", fiber.Map{
		"Title":          Title,
		"Query":          query,
		"ProviderScope":  providerScope,
		"Channels":       channelResults,
		"AppGroups":      appGroups,
		"PremiumResults": premiumResults,
		"IndexComplete":  television.PremiumSearchIndexComplete(),
		"IsNotLoggedIn":  !utils.CheckLoggedIn(),
	})
}

// vodEpisodeView carries one episode to the template with a ready-to-use play
// link. Episode content IDs play through stream_type=vod (HLS).
type vodEpisodeView struct {
	Season      int
	EpisodeNo   int
	Name        string
	Description string
	ImageURL    string
	DurationMin int
	PlayURL     string
}

// premiumContentPlayURL builds the play URL for a content_id-bearing item
// (episode, movie, or standalone asset). The playback API resolves these via
// stream_type=provider with channel_id carrying the content ID, which returns
// the Widevine MPD the DRM player handles.
func premiumContentPlayURL(providerID, contentID string) string {
	return "/premium/providers/" + url.PathEscape(providerID) + "/play?streamType=provider&channelId=" + url.QueryEscape(contentID)
}

// PremiumProviderItemHandler renders the detail page for one premium catalog
// item. When the VOD metadata API knows the content ID, the page shows the
// app's show experience — season selector plus the episode roll — otherwise it
// falls back to artwork, description and related items.
func PremiumProviderItemHandler(c *fiber.Ctx) error {
	providerID := strings.TrimSpace(c.Params("id"))
	itemID := strings.TrimSpace(c.Params("itemId"))

	match, related := television.PremiumProviderItem(providerID, itemID)

	contentID := itemID
	if match != nil && match.Item.ContentID != "" {
		contentID = match.Item.ContentID
	}

	// The metadata API is also the fallback for items missing from the search
	// index: a playable page can still be rendered from it alone.
	meta, _ := television.PremiumProviderVodMetadata(contentID, 0)

	if match == nil && meta == nil {
		return c.Render("views/premium_item", fiber.Map{
			"Title":         Title,
			"ProviderID":    providerID,
			"ItemID":        itemID,
			"NotFound":      true,
			"IsNotLoggedIn": !utils.CheckLoggedIn(),
		})
	}

	// Season picker: the API lists seasons newest-first and caps the default
	// episode roll, so every request fetches the selected season explicitly.
	seasons := make([]int, 0)
	if meta != nil {
		seasons = append(seasons, meta.SeasonList...)
		sort.Ints(seasons)
	}
	selectedSeason := 0
	episodes := make([]vodEpisodeView, 0)
	if len(seasons) > 0 {
		selectedSeason, _ = strconv.Atoi(c.Query("season"))
		valid := false
		for _, season := range seasons {
			if season == selectedSeason {
				valid = true
				break
			}
		}
		if !valid {
			selectedSeason = seasons[0]
		}
		if seasonMeta, err := television.PremiumProviderVodMetadata(contentID, selectedSeason); err == nil && seasonMeta != nil {
			sort.Slice(seasonMeta.Episodes, func(i, j int) bool {
				return seasonMeta.Episodes[i].EpisodeNo < seasonMeta.Episodes[j].EpisodeNo
			})
			for _, episode := range seasonMeta.Episodes {
				episodes = append(episodes, vodEpisodeView{
					Season:      episode.Season,
					EpisodeNo:   episode.EpisodeNo,
					Name:        episode.Name,
					Description: episode.Description,
					ImageURL:    episode.PosterURL(),
					DurationMin: (episode.TotalDuration + 30) / 60,
					PlayURL:     premiumContentPlayURL(providerID, episode.ContentID),
				})
			}
		}
	}

	providerName := providerID
	heroTitle := "Premium item"
	heroSubtitle := ""
	heroDescription := ""
	heroImage := ""
	heroTags := make([]string, 0)
	playURL := premiumContentPlayURL(providerID, contentID)
	if meta != nil {
		if meta.Provider != "" {
			providerName = meta.Provider
		}
		if meta.Name != "" {
			heroTitle = meta.Name
		}
		heroDescription = meta.Description
		heroImage = meta.PosterURL()
		if meta.Language != "" {
			heroSubtitle = meta.Language
		}
		heroTags = append(heroTags, meta.Genres...)
	}
	if match != nil {
		if match.ProviderName != "" {
			providerName = match.ProviderName
		}
		if match.Item.Title != "" {
			heroTitle = match.Item.Title
		}
		if match.Item.Subtitle != "" {
			heroSubtitle = match.Item.Subtitle
		}
		if match.Item.Description != "" && heroDescription == "" {
			heroDescription = match.Item.Description
		}
		if match.Item.ImageURL != "" {
			heroImage = match.Item.ImageURL
		}
		if len(match.Item.Tags) > 0 {
			heroTags = match.Item.Tags
		}
		if meta == nil {
			playURL = premiumItemPlayURL(providerID, match.Item)
		}
	}
	// On a show page the hero Play action starts the first listed episode.
	if len(episodes) > 0 {
		playURL = episodes[0].PlayURL
	}

	relatedViews := make([]premiumSearchResultView, 0, len(related))
	for _, relatedMatch := range related {
		relatedViews = append(relatedViews, premiumResultView(relatedMatch))
	}

	return c.Render("views/premium_item", fiber.Map{
		"Title":           Title,
		"ProviderID":      providerID,
		"ProviderName":    providerName,
		"HeroTitle":       heroTitle,
		"HeroSubtitle":    heroSubtitle,
		"HeroDescription": heroDescription,
		"HeroImage":       heroImage,
		"HeroTags":        heroTags,
		"PlayURL":         playURL,
		"Seasons":         seasons,
		"SelectedSeason":  selectedSeason,
		"Episodes":        episodes,
		"Related":         relatedViews,
		"IsNotLoggedIn":   !utils.CheckLoggedIn(),
	})
}
