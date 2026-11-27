package handlers

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jiotv-go/jiotv_go/v3/pkg/television"
	"github.com/jiotv-go/jiotv_go/v3/pkg/utils"
)

// appHomeTabs mirrors the app's top tab bar. The IDs come from the app's
// home/get tabid parameter; the labels reflect the content each tab serves.
var appHomeTabs = []struct {
	ID   int
	Name string
}{
	{1, "For You"},
	{0, "Movies"},
	{2, "Shows"},
	{3, "Explore"},
	{4, "Sports"},
	{5, "News"},
	{6, "Kids"},
}

// appRailItemView carries one rail card with its resolved link and artwork.
type appRailItemView struct {
	Title       string
	Subtitle    string
	ImageURL    string
	URL         string
	IsPremium   bool
	IsLiveNow   bool
	ChannelName string
}

// appRailView pairs a rail's heading with its resolved items.
type appRailView struct {
	Name      string
	SeeAllURL string
	Items     []appRailItemView
}

// appItemURL maps a rail item to its in-app destination, mirroring the app's
// own routing: VOD clips open the detail page, channel tiles play live, and
// programme tiles play live-or-catchup depending on the air window.
func appItemURL(item television.AppRailItem, railType string, now time.Time) string {
	if contentID := item.VODContentID(); contentID != "" {
		provider := strings.TrimSpace(string(item.Provider))
		if provider == "" {
			provider = "premium"
		}
		return "/premium/providers/" + url.PathEscape(provider) + "/item/" + url.PathEscape(contentID)
	}

	channelID := item.ChannelIDStr()
	if channelID == "" || channelID == "0" {
		return ""
	}
	if bool(item.IsChannel) || railType == "channel" || railType == "channels" {
		return "/play/" + url.PathEscape(channelID)
	}
	if item.AiringNow(now) {
		return "/play/" + url.PathEscape(channelID)
	}
	// Catchup URLs only resolve on channels with catchup rights; other
	// programmes fall back to the live stream rather than a 403.
	canCatchup := bool(item.IsCatchupAvailable) || bool(item.IsPastEpisode)
	if srno := item.SrnoStr(); canCatchup && srno != "" && srno != "0" && item.StartEpoch > 0 {
		return "/catchup/play/" + url.PathEscape(channelID) +
			"?start=" + item.CatchupStartEpochStr() +
			"&end=" + item.CatchupEndEpochStr() +
			"&srno=" + url.QueryEscape(srno) +
			"&showname=" + url.QueryEscape(item.Title()) +
			"&description=" + url.QueryEscape(string(item.EpisodeDesc)) +
			"&poster=" + url.QueryEscape(string(item.EpisodePoster))
	}
	return "/play/" + url.PathEscape(channelID)
}

// appItemSubtitle shows channel + timing context under the card title.
func appItemSubtitle(item television.AppRailItem, now time.Time) string {
	parts := make([]string, 0, 2)
	if string(item.ChannelName) != "" && string(item.ChannelName) != item.Title() {
		parts = append(parts, string(item.ChannelName))
	}
	if string(item.Vendor) != "" {
		parts = append(parts, string(item.Vendor))
	}
	if item.StartEpoch > 0 {
		loc, err := time.LoadLocation("Asia/Kolkata")
		if err != nil {
			loc = time.FixedZone("IST", 5*3600+30*60)
		}
		parts = append(parts, time.UnixMilli(int64(item.StartEpoch)).In(loc).Format("02 Jan, 03:04 PM"))
	}
	return strings.Join(parts, " • ")
}

func appRailItems(rail television.AppRail, now time.Time) []appRailItemView {
	items := make([]appRailItemView, 0, len(rail.Items))
	for _, item := range rail.Items {
		link := appItemURL(item, rail.Type, now)
		if link == "" {
			continue
		}
		items = append(items, appRailItemView{
			Title:       item.Title(),
			Subtitle:    appItemSubtitle(item, now),
			ImageURL:    item.ImageURL(),
			URL:         link,
			IsPremium:   bool(item.IsPremium),
			IsLiveNow:   item.AiringNow(now),
			ChannelName: string(item.ChannelName),
		})
	}
	return items
}

// AppHomeHandler renders the app's home screen: personalised For-You rails on
// the default tab plus the selected tab's curated rails, exactly like the
// mobile app's top-tab layout.
func AppHomeHandler(c *fiber.Ctx) error {
	tabID, err := strconv.Atoi(strings.TrimSpace(c.Query("tab", "1")))
	if err != nil {
		tabID = 1
	}

	now := time.Now()
	rails := make([]appRailView, 0)
	pageError := ""

	// The landing tab leads with local Continue Watching, then the
	// personalised For-You rails — matching the app's home layout.
	if tabID == 1 {
		if items := recentWatchItems(20); len(items) > 0 {
			rails = append(rails, appRailView{Name: "Continue Watching", Items: items})
		}
		if forYou, fyErr := television.AppForYou(); fyErr == nil && forYou != nil {
			for _, rail := range forYou.Rails {
				items := appRailItems(rail, now)
				if len(items) > 0 {
					rails = append(rails, appRailView{Name: rail.Name, Items: items})
				}
			}
		}
	}

	if home, homeErr := television.AppHomeTab(tabID, 0); homeErr == nil && home != nil {
		for _, rail := range home.Rails {
			items := appRailItems(rail, now)
			if len(items) == 0 {
				continue
			}
			view := appRailView{Name: rail.Name, Items: items}
			if rail.CatID != "" {
				view.SeeAllURL = "/home/seeall?cat_id=" + url.QueryEscape(rail.CatID) + "&name=" + url.QueryEscape(rail.Name)
			}
			rails = append(rails, view)
		}
	} else {
		utils.Log.Printf("App home tab %d fetch failed: %v", tabID, homeErr)
		pageError = "Could not load home rails right now"
	}

	// Spotlight hero: first image-backed item from the first content rail
	// (skipping Continue Watching, which is personal rather than editorial).
	var hero *appRailItemView
	for _, rail := range rails {
		if rail.Name == "Continue Watching" {
			continue
		}
		for i := range rail.Items {
			if rail.Items[i].ImageURL != "" {
				hero = &rail.Items[i]
				break
			}
		}
		if hero != nil {
			break
		}
	}

	return c.Render("views/app_home", fiber.Map{
		"Title":         Title,
		"Tabs":          appHomeTabs,
		"SelectedTab":   tabID,
		"Rails":         rails,
		"Hero":          hero,
		"Error":         pageError,
		"IsNotLoggedIn": !utils.CheckLoggedIn(),
	})
}

// AppSeeAllHandler expands one rail into a full grid via the app's
// categoryseeall endpoint — the "See All" target on the app home screen.
func AppSeeAllHandler(c *fiber.Ctx) error {
	catID := strings.TrimSpace(c.Query("cat_id"))
	name := strings.TrimSpace(c.Query("name"))

	now := time.Now()
	rails := make([]appRailView, 0)
	items := make([]appRailItemView, 0)
	pageError := ""

	result, err := television.AppCategorySeeAll(catID)
	if err != nil {
		utils.Log.Printf("See-all fetch failed for %s: %v", catID, err)
		pageError = "Could not load this category right now"
	} else if result != nil {
		for _, rail := range result.Rails {
			railItems := appRailItems(rail, now)
			if rail.Name != "" && rail.Name != name && len(rails) == 0 && len(result.Rails) > 1 {
				rails = append(rails, appRailView{Name: rail.Name, Items: railItems})
				continue
			}
			items = append(items, railItems...)
		}
	}

	return c.Render("views/app_home", fiber.Map{
		"Title":         Title,
		"SeeAll":        true,
		"SeeAllName":    name,
		"Rails":         rails,
		"Items":         items,
		"Error":         pageError,
		"IsNotLoggedIn": !utils.CheckLoggedIn(),
	})
}
