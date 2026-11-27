package television

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	pkgutils "github.com/jiotv-go/jiotv_go/v3/pkg/utils"
	"github.com/valyala/fasthttp"
)

// appAPIBase hosts the app's non-CDN API surface discovered in the JioTV app
// (v7.1.8): home tab rails, For-You rails, typed search, and see-all pages.
const appAPIBase = "https://tv.media.jio.com/apis/"

// appAPITabIDs are the home-screen top tabs the app offers, ordered as the
// app's tab bar does.
var appAPITabIDs = []int{1, 0, 2, 3, 4, 5, 6}

// AppHomeTabIDs returns the tab IDs the app home screen supports.
func AppHomeTabIDs() []int {
	ids := make([]int, len(appAPITabIDs))
	copy(ids, appAPITabIDs)
	return ids
}

// appBool accepts the app's inconsistent boolean encodings — true/false,
// "true"/"false", and 1/0 all appear in different payloads.
type appBool bool

func (b *appBool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.ToLower(string(data)), `"`)
	*b = appBool(s == "true" || s == "1")
	return nil
}

// appString accepts string, number or bool scalars — fields like content_id
// are strings in vod payloads but numbers in games payloads.
type appString string

func (s *appString) UnmarshalJSON(data []byte) error {
	raw := string(data)
	if raw == "null" {
		*s = ""
		return nil
	}
	*s = appString(strings.Trim(raw, `"`))
	return nil
}

// appInt accepts both numeric and quoted-string integers; epoch fields arrive
// inconsistently typed across endpoints.
type appInt int64

func (i *appInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Some payloads carry float literals (e.g. 1795761000000.0).
		if f, ferr := strconv.ParseFloat(s, 64); ferr == nil {
			v = int64(f)
		} else {
			v = 0
		}
	}
	*i = appInt(v)
	return nil
}

// AppRailItem is one card inside a home rail or app-search group. TV-side
// items are EPG programme objects (channel_id + srno + epochs); VOD-side
// items carry a contentId. Both share the same JSON shape upstream.
type AppRailItem struct {
	ChannelID          json.Number `json:"channel_id"`
	ChannelName        appString   `json:"channel_name"`
	LogoURL            appString   `json:"logoUrl"`
	Srno               json.Number `json:"srno"`
	ShowID             appString   `json:"showId"`
	ShowName           appString   `json:"showname"`
	ShowNameAlt        appString   `json:"showName"`
	StartEpoch         appInt      `json:"startEpoch"`
	EndEpoch           appInt      `json:"endEpoch"`
	Showtime           appString   `json:"showtime"`
	EpisodePoster      appString   `json:"episodePoster"`
	EpisodeThumb       appString   `json:"episodeThumbnail"`
	Description        appString   `json:"description"`
	EpisodeDesc        appString   `json:"episode_desc"`
	Duration           appInt      `json:"duration"`
	IsCatchupAvailable appBool     `json:"isCatchupAvailable"`
	IsLiveAvailable    appBool     `json:"isLiveAvailable"`
	IsPastEpisode      appBool     `json:"isPastEpisode"`
	IsPremium          appBool     `json:"is_premium"`
	IsChannel          appBool     `json:"isChannel"`
	PlanType           appString   `json:"plan_type"`

	ContentID    appString `json:"contentId"`
	ContentIDAlt appString `json:"content_id"`
	ClipName     appString `json:"clip_name"`
	ClipThumb    appString `json:"clip_thumbnail"`
	Provider     appString `json:"provider"`
	Vendor       appString `json:"vendor"`
	ContentType  appString `json:"contentType"`
}

// ChannelIDStr normalises the channel_id field to a string.
func (item AppRailItem) ChannelIDStr() string { return item.ChannelID.String() }

// SrnoStr normalises srno to a string for catchup URLs.
func (item AppRailItem) SrnoStr() string { return item.Srno.String() }

// VODContentID returns the content ID whichever field upstream populated.
func (item AppRailItem) VODContentID() string {
	if item.ContentID != "" {
		return string(item.ContentID)
	}
	return string(item.ContentIDAlt)
}

// Title picks the best display name across EPG and VOD shapes.
func (item AppRailItem) Title() string {
	for _, title := range []string{string(item.ShowName), string(item.ShowNameAlt), string(item.ClipName), string(item.ChannelName)} {
		if strings.TrimSpace(title) != "" {
			return strings.TrimSpace(title)
		}
	}
	return ""
}

// ImageURL resolves the item artwork to an absolute CDN URL. EPG posters live
// under dare_images/shows, channel logos under dare_images/images, and VOD
// clip thumbs under the jiotvimages root.
func (item AppRailItem) ImageURL() string {
	for _, candidate := range []string{string(item.EpisodePoster), string(item.EpisodeThumb)} {
		if path := strings.TrimSpace(candidate); path != "" {
			if strings.HasPrefix(path, "http") {
				return path
			}
			return "https://jiotvimages.cdn.jio.com/dare_images/shows/" + path
		}
	}
	if thumb := strings.TrimSpace(string(item.ClipThumb)); thumb != "" {
		if strings.HasPrefix(thumb, "http") {
			return thumb
		}
		return "https://jiotvimages.cdn.jio.com/" + strings.TrimLeft(thumb, "/")
	}
	if logo := strings.TrimSpace(string(item.LogoURL)); logo != "" {
		if strings.HasPrefix(logo, "http") {
			return logo
		}
		return "https://jiotvimages.cdn.jio.com/dare_images/images/" + logo
	}
	return ""
}

// AiringNow reports whether the programme is in its live window right now.
func (item AppRailItem) AiringNow(now time.Time) bool {
	nowMillis := now.UnixMilli()
	return item.StartEpoch > 0 && int64(item.StartEpoch) <= nowMillis && int64(item.EndEpoch) > nowMillis
}

// AppRail is one horizontal row on the app home screen.
type AppRail struct {
	Name  string        `json:"name"`
	Type  string        `json:"type"`
	CatID string        `json:"cat_id"`
	Items []AppRailItem `json:"data"`
}

// AppHomeResponse is the v3.9 home/get payload.
type AppHomeResponse struct {
	Code       int       `json:"code"`
	Count      int       `json:"count"`
	TotalPages int       `json:"totalPages"`
	Rails      []AppRail `json:"featuredNewData"`
}

// AppSearchResponse is the v2.1 search/search payload; the account's AVOD
// flag routes the app to this version.
type AppSearchResponse struct {
	Code int `json:"code"`
	Data struct {
		Live     []AppRailItem `json:"live"`
		Catchup  []AppRailItem `json:"catchup"`
		Channels []AppRailItem `json:"channels"`
		Videos   []AppRailItem `json:"videos"`
		Movies   []AppRailItem `json:"movies"`
		Shows    []AppRailItem `json:"shows"`
		Games    []AppRailItem `json:"games"`
	} `json:"data"`
}

// appAPICacheTTL keeps rails/search responses fresh without hammering the API
// on every page switch; rails are personalised but change slowly.
const appAPICacheTTL = 5 * time.Minute

var appAPICache sync.Map // key: endpoint+params -> appAPICacheEntry

type appAPICacheEntry struct {
	body      []byte
	expiresAt time.Time
}

// appAPIGet performs an authenticated GET against the app API host with a
// short body cache. The app sends the same plain-token header set the
// provider catalog APIs already use.
func appAPIGet(path string, out interface{}) error {
	cacheKey := path
	if cached, ok := appAPICache.Load(cacheKey); ok {
		if entry, ok := cached.(appAPICacheEntry); ok && time.Now().Before(entry.expiresAt) {
			return json.Unmarshal(entry.body, out)
		}
		appAPICache.Delete(cacheKey)
	}

	credentials, err := pkgutils.GetJIOTVCredentials()
	if err != nil || credentials == nil {
		return errors.New("not logged in")
	}

	resp, err := pkgutils.MakeHTTPRequest(pkgutils.HTTPRequestConfig{
		URL:     appAPIBase + path,
		Method:  "GET",
		Headers: buildAuthenticatedHeaders(credentials),
	}, pkgutils.GetRequestClient())
	if err != nil {
		return err
	}
	defer fasthttp.ReleaseResponse(resp)

	if resp.StatusCode() == fasthttp.StatusNoContent || resp.StatusCode() == fasthttp.StatusNotFound {
		return nil
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		return fmt.Errorf("app API %s failed with status code: %d", path, resp.StatusCode())
	}

	body := make([]byte, len(resp.Body()))
	copy(body, resp.Body())
	appAPICache.Store(cacheKey, appAPICacheEntry{body: body, expiresAt: time.Now().Add(appAPICacheTTL)})
	return json.Unmarshal(body, out)
}

// AppHomeTab fetches one home tab's rails (v3.9/home/get). tabid 1 is the
// app's primary landing tab; the remaining IDs come from AppHomeTabIDs.
func AppHomeTab(tabID, page int) (*AppHomeResponse, error) {
	var result AppHomeResponse
	path := fmt.Sprintf("v3.9/home/get?page=%d&tabid=%d", page, tabID)
	if err := appAPIGet(path, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AppForYou fetches the personalised "Programs/Channels You Watch" section
// shown above the rails on the app's home tab.
func AppForYou() (*AppHomeResponse, error) {
	var envelope struct {
		Code int       `json:"code"`
		Data []AppRail `json:"data"`
	}
	if err := appAPIGet("v1.4/foryousection/get", &envelope); err != nil {
		return nil, err
	}
	return &AppHomeResponse{Code: envelope.Code, Rails: envelope.Data}, nil
}

// AppCategorySeeAll expands a rail into its full grid via the app's
// see-all endpoint (v3.0/tab/categoryseeall). Unlike home/get, this endpoint
// nests its rails under data rather than featuredNewData.
func AppCategorySeeAll(catID string) (*AppHomeResponse, error) {
	catID = strings.TrimSpace(catID)
	if catID == "" {
		return nil, errors.New("cat_id is required")
	}
	var envelope struct {
		Code  int       `json:"code"`
		Count int       `json:"count"`
		Rails []AppRail `json:"data"`
	}
	path := "v3.0/tab/categoryseeall?cat_id=" + url.QueryEscape(catID) + "&userLanguages=english"
	if err := appAPIGet(path, &envelope); err != nil {
		return nil, err
	}
	return &AppHomeResponse{Code: envelope.Code, Count: envelope.Count, Rails: envelope.Rails}, nil
}

// AppSearch runs the app's real search (v2.1/search/search) — the same query
// that fills the mobile app's Live/Catchup/Channels/Videos tabs. The type
// filter is sent empty, matching the app's "all" tab.
func AppSearch(query string) (*AppSearchResponse, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query is required")
	}
	var result AppSearchResponse
	path := "v2.1/search/search?query=" + url.QueryEscape(query) + "&type="
	if err := appAPIGet(path, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CatchupStartEpochStr returns startEpoch as a decimal string for URLs.
func (item AppRailItem) CatchupStartEpochStr() string {
	return strconv.FormatInt(int64(item.StartEpoch), 10)
}

// CatchupEndEpochStr returns endEpoch as a decimal string for URLs.
func (item AppRailItem) CatchupEndEpochStr() string {
	return strconv.FormatInt(int64(item.EndEpoch), 10)
}
