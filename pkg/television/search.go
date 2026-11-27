package television

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jiotv-go/jiotv_go/v3/pkg/store"
	"github.com/jiotv-go/jiotv_go/v3/pkg/utils"
	"github.com/valyala/fasthttp"
)

// PremiumSearchResult pairs a premium catalog item with its provider so the
// caller can render a playable search result.
type PremiumSearchResult struct {
	ProviderID   string
	ProviderName string
	Item         PremiumProviderCatalogItem
}

const (
	// premiumCatalogPageSize is the page size the upstream browse API honours;
	// larger values are clamped by the API itself.
	premiumCatalogPageSize = 30
	// premiumCatalogMaxPages is how many pages are fetched per browse bucket.
	// The upstream browse API hard-caps results at 15 pages (~450 items) and
	// returns empty pages beyond that.
	premiumCatalogMaxPages = 15
	// premiumSearchIndexTTL is how long a completed deep index is trusted
	// before the next search triggers a background rebuild. Provider catalogs
	// change slowly, so a long TTL keeps searches instant.
	premiumSearchIndexTTL = 30 * time.Minute
	// premiumIndexBucketConcurrency bounds concurrent upstream browse requests
	// while the index is being built.
	premiumIndexBucketConcurrency = 10
)

// premiumIndexEntry is one deduplicated catalog item in the search index. The
// precomputed haystack keeps repeated searches cheap.
type premiumIndexEntry struct {
	result   PremiumSearchResult
	haystack string
}

// premiumIndexTask describes one browse bucket to crawl: a provider plus the
// filter query map (empty for the unfiltered bucket, or a single genre /
// contentType value). Every bucket yields up to premiumCatalogMaxPages pages,
// so unioning all filter buckets covers far more of a provider's catalog than
// the ~450 items the unfiltered browse exposes.
type premiumIndexTask struct {
	catalogProviderID string
	providerName      string
	queryMap          map[string]string
}

var premiumSearchIndex = struct {
	sync.RWMutex
	items    map[string]premiumIndexEntry
	builtAt  time.Time
	building bool
	deepDone bool
}{items: map[string]premiumIndexEntry{}}

// PremiumSearchIndexComplete reports whether the deep catalog crawl has
// finished. The template uses it to hint that results may still be partial.
func PremiumSearchIndexComplete() bool {
	premiumSearchIndex.RLock()
	defer premiumSearchIndex.RUnlock()
	return premiumSearchIndex.deepDone
}

// SearchChannels returns channels whose name, category or language contains
// query (case-insensitive).
func SearchChannels(query string) []Channel {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}

	response, err := Channels()
	if err != nil {
		return nil
	}

	matches := make([]Channel, 0)
	for _, channel := range response.Result {
		if strings.Contains(strings.ToLower(channel.Name), query) ||
			strings.Contains(strings.ToLower(CategoryMap[channel.Category]), query) ||
			strings.Contains(strings.ToLower(LanguageMap[channel.Language]), query) {
			matches = append(matches, channel)
		}
	}
	return matches
}

// normalizeSearchText folds a string to lowercase alphanumeric words separated
// by single spaces. Titles carry punctuation ("Man vs. Wild") that a plain
// substring query ("man vs wil") would otherwise never match.
func normalizeSearchText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastWasSpace = false
		} else if !lastWasSpace {
			b.WriteByte(' ')
			lastWasSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// SearchPremiumProviders searches the premium catalog index for items whose
// title, subtitle or description contains query (case-insensitive).
//
// The upstream browse API has no title-search parameter and caps any bucket at
// 15 pages, so coverage is built by crawling: the first search runs a fast
// synchronous pass over each provider's unfiltered bucket (~450 items each),
// then a background deep crawl walks every genre and contentType bucket to
// cover most of the catalog. Subsequent searches are instant and the index
// rebuilds in the background once it is older than premiumSearchIndexTTL.
func SearchPremiumProviders(query string) []PremiumSearchResult {
	query = normalizeSearchText(query)
	if query == "" || store.KVS == nil {
		return nil
	}

	credentials, err := utils.GetJIOTVCredentials()
	if err != nil || credentials == nil || credentials.AccessToken == "" {
		return nil
	}

	providers, err := PremiumProviders()
	if err != nil || len(providers) == 0 {
		return nil
	}

	client := utils.GetRequestClient()
	ensurePremiumSearchIndex(client, credentials, providers)

	premiumSearchIndex.RLock()
	defer premiumSearchIndex.RUnlock()
	matches := make([]PremiumSearchResult, 0)
	for _, entry := range premiumSearchIndex.items {
		if strings.Contains(entry.haystack, query) {
			matches = append(matches, entry.result)
		}
	}
	return matches
}

// premiumRelatedLimit caps how many related items a detail page shows.
const premiumRelatedLimit = 24

// premiumTitleStopwords are tokens ignored when comparing item titles for
// relatedness; they appear across unrelated titles and add only noise.
var premiumTitleStopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "of": {}, "and": {}, "or": {}, "vs": {},
	"with": {}, "in": {}, "on": {}, "at": {}, "to": {}, "for": {}, "ep": {},
	"episode": {}, "season": {}, "part": {}, "new": {}, "full": {}, "hd": {},
}

// PremiumProviderItem returns one indexed catalog item plus related items from
// the same provider. Relatedness ranks shared metadata tags/genres highest
// (items for one show share show-level tags) and falls back to shared title
// tokens (covers providers whose catalog entries carry no tag metadata).
func PremiumProviderItem(providerID, itemID string) (*PremiumSearchResult, []PremiumSearchResult) {
	providerID = strings.TrimSpace(providerID)
	itemID = strings.TrimSpace(itemID)
	if providerID == "" || itemID == "" || store.KVS == nil {
		return nil, nil
	}

	credentials, err := utils.GetJIOTVCredentials()
	if err != nil || credentials == nil || credentials.AccessToken == "" {
		return nil, nil
	}
	providers, err := PremiumProviders()
	if err != nil {
		return nil, nil
	}
	client := utils.GetRequestClient()
	ensurePremiumSearchIndex(client, credentials, providers)

	premiumSearchIndex.RLock()
	defer premiumSearchIndex.RUnlock()

	entry, found := premiumSearchIndex.items[providerID+"|"+itemID]
	if !found {
		return nil, nil
	}

	item := entry.result.Item
	tagSet := make(map[string]struct{}, len(item.Tags))
	for _, tag := range item.Tags {
		tagSet[strings.ToLower(tag)] = struct{}{}
	}
	titleTokens := premiumTitleTokens(item.Title)

	type scored struct {
		result PremiumSearchResult
		score  int
	}
	candidates := make([]scored, 0)
	for key, candidate := range premiumSearchIndex.items {
		if !strings.HasPrefix(key, providerID+"|") || key == providerID+"|"+itemID {
			continue
		}
		score := 0
		for _, tag := range candidate.result.Item.Tags {
			if _, shared := tagSet[strings.ToLower(tag)]; shared {
				score += 3
			}
		}
		for token := range premiumTitleTokens(candidate.result.Item.Title) {
			if _, shared := titleTokens[token]; shared {
				score++
			}
		}
		if score > 0 {
			candidates = append(candidates, scored{result: candidate.result, score: score})
		}
	}

	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	related := make([]PremiumSearchResult, 0, premiumRelatedLimit)
	for _, candidate := range candidates {
		if len(related) >= premiumRelatedLimit {
			break
		}
		related = append(related, candidate.result)
	}

	result := entry.result
	return &result, related
}

// premiumTitleTokens splits a title into lowercase words for overlap scoring.
func premiumTitleTokens(title string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, word := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(word) < 2 {
			continue
		}
		if _, stopword := premiumTitleStopwords[word]; stopword {
			continue
		}
		tokens[word] = struct{}{}
	}
	return tokens
}

// ensurePremiumSearchIndex starts index work when needed. On the very first
// build it restores the persisted index from disk (instant full coverage),
// then runs the shallow (unfiltered) pass synchronously so the caller's
// search already sees fresh top catalog entries, and finishes the deep crawl
// in the background. Once a deep index exists it is rebuilt in the background
// after the TTL while the stale index keeps serving searches.
func ensurePremiumSearchIndex(client *fasthttp.Client, credentials *utils.JIOTV_CREDENTIALS, providers []PremiumProvider) {
	premiumSearchIndex.Lock()
	if premiumSearchIndex.building ||
		(premiumSearchIndex.deepDone && time.Since(premiumSearchIndex.builtAt) <= premiumSearchIndexTTL) {
		premiumSearchIndex.Unlock()
		return
	}
	premiumSearchIndex.building = true
	firstBuild := !premiumSearchIndex.deepDone
	premiumSearchIndex.Unlock()

	if firstBuild {
		loadPremiumIndexFromDisk()
		runPremiumIndexTasks(client, credentials, shallowPremiumIndexTasks(client, credentials, providers))
	}
	go func() {
		runPremiumIndexTasks(client, credentials, allPremiumIndexTasks(client, credentials, providers))
		premiumSearchIndex.Lock()
		premiumSearchIndex.deepDone = true
		premiumSearchIndex.builtAt = time.Now()
		premiumSearchIndex.building = false
		premiumSearchIndex.Unlock()
		savePremiumIndexToDisk()
	}()
}

// shallowPremiumIndexTasks builds one unfiltered browse task per provider.
func shallowPremiumIndexTasks(client *fasthttp.Client, credentials *utils.JIOTV_CREDENTIALS, providers []PremiumProvider) []premiumIndexTask {
	tasks := make([]premiumIndexTask, 0, len(providers))
	for _, provider := range providers {
		catalogProviderID := resolveProviderForIndex(client, credentials, provider.ProviderID)
		if catalogProviderID == "" {
			continue
		}
		tasks = append(tasks, premiumIndexTask{
			catalogProviderID: catalogProviderID,
			providerName:      provider.Name,
			queryMap:          map[string]string{},
		})
	}
	return tasks
}

// allPremiumIndexTasks builds the unfiltered task plus one task per genre and
// contentType filter value exposed by the provider configuration. The browse
// API filters server-side on these parameters, so each bucket reaches catalog
// entries the plain browse window cannot.
func allPremiumIndexTasks(client *fasthttp.Client, credentials *utils.JIOTV_CREDENTIALS, providers []PremiumProvider) []premiumIndexTask {
	configHeaders := buildProviderConfigHeaders(credentials)
	tasks := shallowPremiumIndexTasks(client, credentials, providers)
	seen := make(map[string]struct{}, len(tasks))

	for _, provider := range providers {
		catalogProviderID := resolveProviderForIndex(client, credentials, provider.ProviderID)
		if catalogProviderID == "" {
			continue
		}

		filterResponse, err := fetchPremiumProviderFilterFromAPI(client, catalogProviderID, configHeaders)
		if err != nil {
			utils.SafeLogf("Unable to fetch provider filters for search index %s: %v", catalogProviderID, err)
			continue
		}

		for _, providerFilters := range filterResponse.Data.Provider {
			for _, filter := range providerFilters.Filters {
				filterName := strings.ToLower(strings.TrimSpace(filter.FilterName))
				if filterName != "genre" && filterName != "contenttype" {
					continue
				}
				for _, value := range filter.Values {
					key := strings.TrimSpace(value.Key)
					if key == "" {
						continue
					}
					dedupeKey := catalogProviderID + "|" + filterName + "|" + strings.ToLower(key)
					if _, exists := seen[dedupeKey]; exists {
						continue
					}
					seen[dedupeKey] = struct{}{}
					tasks = append(tasks, premiumIndexTask{
						catalogProviderID: catalogProviderID,
						providerName:      provider.Name,
						queryMap:          map[string]string{filter.FilterName: key},
					})
				}
			}
		}
	}
	return tasks
}

// resolveProviderForIndex maps an entitlement provider entry to the content
// provider ID the browse API expects.
func resolveProviderForIndex(client *fasthttp.Client, credentials *utils.JIOTV_CREDENTIALS, providerID string) string {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return ""
	}
	return resolveCatalogProviderID(client, credentials, providerID)
}

// runPremiumIndexTasks crawls every bucket concurrently and merges results
// into the index progressively, so searches improve as the build advances.
func runPremiumIndexTasks(client *fasthttp.Client, credentials *utils.JIOTV_CREDENTIALS, tasks []premiumIndexTask) {
	catalogHeaders := buildProviderCatalogHeaders(credentials)
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, premiumIndexBucketConcurrency)

	for _, task := range tasks {
		wg.Add(1)
		go func(task premiumIndexTask) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			mergePremiumIndexItems(task, fetchPremiumIndexBucket(client, task, catalogHeaders))
		}(task)
	}
	wg.Wait()
}

// fetchPremiumIndexBucket walks all pages of one browse bucket.
func fetchPremiumIndexBucket(client *fasthttp.Client, task premiumIndexTask, catalogHeaders map[string]string) []PremiumProviderCatalogItem {
	items := make([]PremiumProviderCatalogItem, 0)
	for page := 0; page < premiumCatalogMaxPages; page++ {
		response, err := fetchPremiumProviderCatalogFromAPI(client, task.catalogProviderID, page, premiumCatalogPageSize, task.queryMap, catalogHeaders)
		if err != nil {
			break
		}
		pageItems := mapPremiumProviderCatalogItems(premiumCatalogData(response), task.catalogProviderID)
		if len(pageItems) == 0 {
			break
		}
		items = append(items, pageItems...)
	}
	return items
}

// mergePremiumIndexItems adds a bucket's items to the index, deduplicating by
// provider + item ID so overlapping genre buckets do not repeat results.
func mergePremiumIndexItems(task premiumIndexTask, items []PremiumProviderCatalogItem) {
	if len(items) == 0 {
		return
	}
	premiumSearchIndex.Lock()
	defer premiumSearchIndex.Unlock()
	for _, item := range items {
		itemKey := item.ID
		if itemKey == "" {
			itemKey = item.Title
		}
		if itemKey == "" {
			continue
		}
		premiumSearchIndex.items[task.catalogProviderID+"|"+itemKey] = premiumIndexEntry{
			result: PremiumSearchResult{
				ProviderID:   task.catalogProviderID,
				ProviderName: task.providerName,
				Item:         item,
			},
			haystack: normalizeSearchText(item.Title + " " + item.Subtitle + " " + item.Description + " " + strings.Join(item.Tags, " ")),
		}
	}
}

// premiumIndexDiskFile is the persisted search index written after every deep
// crawl, so a server restart keeps full catalog coverage instead of starting
// empty.
const premiumIndexDiskFile = "premium_search_index.json"

type persistedPremiumIndex struct {
	BuiltAt int64                 `json:"builtAt"`
	Items   []PremiumSearchResult `json:"items"`
}

func premiumIndexDiskPath() string {
	return filepath.Join(store.GetPathPrefix(), premiumIndexDiskFile)
}

// savePremiumIndexToDisk writes the completed index. Called after each deep
// crawl; failures are logged and ignored — persistence is a convenience, not
// a correctness requirement.
func savePremiumIndexToDisk() {
	premiumSearchIndex.RLock()
	persisted := persistedPremiumIndex{
		BuiltAt: premiumSearchIndex.builtAt.Unix(),
		Items:   make([]PremiumSearchResult, 0, len(premiumSearchIndex.items)),
	}
	for _, entry := range premiumSearchIndex.items {
		persisted.Items = append(persisted.Items, entry.result)
	}
	premiumSearchIndex.RUnlock()

	data, err := json.Marshal(persisted)
	if err != nil {
		utils.SafeLogf("Unable to marshal premium search index: %v", err)
		return
	}
	if err := os.WriteFile(premiumIndexDiskPath(), data, 0o600); err != nil {
		utils.SafeLogf("Unable to persist premium search index: %v", err)
	}
}

// loadPremiumIndexFromDisk restores the persisted index into memory. A stale
// file is still loaded — out-of-date results beat empty ones — but the entry
// timestamps keep the normal TTL/refresh behaviour intact.
func loadPremiumIndexFromDisk() {
	data, err := os.ReadFile(premiumIndexDiskPath())
	if err != nil {
		return
	}
	var persisted persistedPremiumIndex
	if err := json.Unmarshal(data, &persisted); err != nil {
		utils.SafeLogf("Unable to parse persisted premium search index: %v", err)
		return
	}
	if len(persisted.Items) == 0 {
		return
	}

	premiumSearchIndex.Lock()
	defer premiumSearchIndex.Unlock()
	for _, result := range persisted.Items {
		itemKey := result.Item.ID
		if itemKey == "" {
			itemKey = result.Item.Title
		}
		if itemKey == "" {
			continue
		}
		premiumSearchIndex.items[result.ProviderID+"|"+itemKey] = premiumIndexEntry{
			result:   result,
			haystack: normalizeSearchText(result.Item.Title + " " + result.Item.Subtitle + " " + result.Item.Description + " " + strings.Join(result.Item.Tags, " ")),
		}
	}
	if persisted.BuiltAt > 0 {
		premiumSearchIndex.builtAt = time.Unix(persisted.BuiltAt, 0)
	}
	premiumSearchIndex.deepDone = true
	utils.SafeLogf("Premium search index restored from disk: %d items", len(persisted.Items))
}
