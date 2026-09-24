package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ==========================================
// HARDCODED CONFIGURATION
// ==========================================

const (
	SCRAPINGBEE_KEY = "5BD77XOJVG73HJCZW6W1F6F4E22CK3L5ZS8IWLWXUHYEBCR19EG6ZII7GMPAPYEWMTXIOM86UR1KEMF6"
	TG_BOT_TOKEN    = "8719204270:AAG3iDePtYHPD9311xSr0rI4-z5SnxxX6IQ"
	TG_CHAT_ID      = "8189708860"
	WORKERS         = 3000
	TG_UPDATE_SEC   = 5
	TG_MAX_FILE_MB  = 30
)

// ==========================================
// GLOBAL PROGRESS TRACKER
// ==========================================

type Progress struct {
	TotalCandidates int64
	Scraped         int64
	Validated       int64
	ValidFound      int64
	Phase           string
	StartTime       time.Time
	SBRequestsUsed  int64
}

var progress = &Progress{StartTime: time.Now()}

// ==========================================
// HTTP CLIENT
// ==========================================

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        10000,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
	},
	Timeout: 20 * time.Second,
}

// ==========================================
// TELEGRAM HELPERS
// ==========================================

func tgSendText(msg string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TG_BOT_TOKEN)
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id": TG_CHAT_ID, "text": msg, "parse_mode": "HTML",
	})
	resp, err := httpClient.Post(apiURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[TG] Text send failed: %v", err)
		return
	}
	resp.Body.Close()
}

func tgEditMessage(messageID int, msg string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", TG_BOT_TOKEN)
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id": TG_CHAT_ID, "message_id": messageID, "text": msg, "parse_mode": "HTML",
	})
	resp, err := httpClient.Post(apiURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	resp.Body.Close()
}

func tgSendFile(filePath string, caption string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	boundary := "----GoFormBoundary" + strconv.FormatInt(rand.Int63(), 16)
	var body bytes.Buffer
	body.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	body.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=\"chat_id\"\r\n\r\n%s\r\n", TG_CHAT_ID))
	body.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	body.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=\"caption\"\r\n\r\n%s\r\n", caption))
	body.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	body.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=\"document\"; filename=\"%s\"\r\n", "valid_sites.txt"))
	body.WriteString("Content-Type: text/plain\r\n\r\n")
	io.Copy(&body, file)
	body.WriteString(fmt.Sprintf("\r\n--%s--\r\n", boundary))

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", TG_BOT_TOKEN)
	req, _ := http.NewRequest("POST", apiURL, &body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("TG upload failed (%d): %s", resp.StatusCode, string(rb))
	}
	return nil
}

func splitAndSendFiles(basePath string) {
	const maxBytes = TG_MAX_FILE_MB * 1024 * 1024
	data, err := os.ReadFile(basePath)
	if err != nil {
		tgSendText(fmt.Sprintf("❌ Failed to read %s: %v", basePath, err))
		return
	}
	if len(data) <= maxBytes {
		caption := fmt.Sprintf("📁 valid_sites.txt (%.2f MB)", float64(len(data))/(1024*1024))
		if err := tgSendFile(basePath, caption); err != nil {
			tgSendText(fmt.Sprintf("❌ Upload failed: %v", err))
		} else {
			tgSendText("✅ File sent!")
		}
		return
	}

	lines := strings.Split(string(data), "\n")
	var chunk strings.Builder
	partNum, totalParts := 1, 0
	for _, l := range lines {
		if chunk.Len()+len(l)+1 > maxBytes && chunk.Len() > 0 {
			totalParts++
			chunk.Reset()
		}
		chunk.WriteString(l + "\n")
	}
	if chunk.Len() > 0 {
		totalParts++
	}

	chunk.Reset()
	partNum = 1
	for _, l := range lines {
		if chunk.Len()+len(l)+1 > maxBytes && chunk.Len() > 0 {
			path := fmt.Sprintf("valid_sites_part%d.txt", partNum)
			os.WriteFile(path, []byte(chunk.String()), 0644)
			tgSendFile(path, fmt.Sprintf("📁 Part %d/%d", partNum, totalParts))
			os.Remove(path)
			partNum++
			chunk.Reset()
			time.Sleep(500 * time.Millisecond)
		}
		chunk.WriteString(l + "\n")
	}
	if chunk.Len() > 0 {
		path := fmt.Sprintf("valid_sites_part%d.txt", partNum)
		os.WriteFile(path, []byte(chunk.String()), 0644)
		tgSendFile(path, fmt.Sprintf("📁 Part %d/%d", partNum, totalParts))
		os.Remove(path)
	}
	tgSendText(fmt.Sprintf("✅ All %d parts sent!", totalParts))
}

// ==========================================
// LIVE PROGRESS UPDATER
// ==========================================

func startProgressUpdater() int {
	tgSendText("🚀 <b>Shopify Harvester Started</b>\n⏳ Initializing...")
	time.Sleep(1 * time.Second)

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TG_BOT_TOKEN)
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id": TG_CHAT_ID, "text": "⏳ Starting...", "parse_mode": "HTML",
	})
	resp, err := httpClient.Post(apiURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var result struct {
		Result struct{ MessageID int `json:"message_id"` } `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	messageID := result.Result.MessageID

	go func() {
		ticker := time.NewTicker(time.Duration(TG_UPDATE_SEC) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			elapsed := time.Since(progress.StartTime).Seconds()
			scraped := atomic.LoadInt64(&progress.Scraped)
			validated := atomic.LoadInt64(&progress.Validated)
			valid := atomic.LoadInt64(&progress.ValidFound)
			total := atomic.LoadInt64(&progress.TotalCandidates)
			sbUsed := atomic.LoadInt64(&progress.SBRequestsUsed)
			phase := progress.Phase
			rate := float64(0)
			if elapsed > 0 {
				rate = float64(validated) / elapsed
			}
			pBar := ""
			if total > 0 {
				pct := float64(validated) / float64(total) * 100
				filled := int(pct / 5)
				if filled > 20 {
					filled = 20
				}
				pBar = fmt.Sprintf("[%s%s] %.1f%%", strings.Repeat("█", filled), strings.Repeat("░", 20-filled), pct)
			}
			msg := fmt.Sprintf(
				"<b>🔄 Shopify Harvester — Live</b>\n\n"+
					"📍 Phase: <code>%s</code>\n%s\n\n"+
					"📡 Scraped: <b>%d</b>\n"+
					"🔍 Validated: <b>%d / %d</b>\n"+
					"✅ Valid Found: <b>%d</b>\n"+
					"🔑 SB Requests: <b>%d / 1000</b>\n"+
					"⚡ Rate: <b>%.1f/sec</b>\n"+
					"⏱️ Elapsed: <b>%.0fs</b>",
				phase, pBar, scraped, validated, total, valid, sbUsed, rate, elapsed,
			)
			if messageID > 0 {
				tgEditMessage(messageID, msg)
			} else {
				tgSendText(msg)
			}
		}
	}()
	return messageID
}

func stopProgressUpdater(messageID int) {
	elapsed := time.Since(progress.StartTime).Seconds()
	valid := atomic.LoadInt64(&progress.ValidFound)
	total := atomic.LoadInt64(&progress.TotalCandidates)
	msg := fmt.Sprintf(
		"<b>✅ Harvest Complete!</b>\n\n"+
			"✅ Valid Sites: <b>%d</b>\n📊 Scanned: <b>%d</b>\n⏱️ Time: <b>%.1fs</b>\n\n<i>Sending file(s)...</i>",
		valid, total, elapsed,
	)
	if messageID > 0 {
		tgEditMessage(messageID, msg)
	} else {
		tgSendText(msg)
	}
}

// ==========================================
// SOURCE 1: SCRAPINGBEE GOOGLE DORKS (FIXED PARSING)
// ==========================================

var HIGH_VALUE_DORKS = []string{
	`site:myshopify.com "add to cart" -site:shopify.com`,
	`"powered by shopify" "add to cart" -site:shopify.com -site:myshopify.com`,
	`site:myshopify.com inurl:products`,
	`"checkout.shopify.com" "add to cart"`,
	`site:myshopify.com "free shipping"`,
	`intitle:"shopify" inurl:collections "add to cart"`,
	`site:myshopify.com "sale" OR "discount" OR "clearance"`,
	`"shopify.com/products" "add to cart" -site:shopify.com`,
	`site:myshopify.com "subscribe" OR "subscription"`,
	`inanchor:"shopify" "add to cart" -site:shopify.com`,
	`site:*.myshopify.com -site:shopify.com`,
	`"myshopify.com" "cart" "checkout" -site:shopify.com`,
	`site:myshopify.com "collections/all"`,
	`"cdn.shopify.com" "add to cart" -site:shopify.com`,
	`site:myshopify.com "product" "price"`,
}

func scrapeGoogleDorks() []string {
	progress.Phase = "Google Dorks (ScrapingBee)"
	var mu sync.Mutex
	var results []string
	sem := make(chan struct{}, 5) // Conservative concurrency for API
	var wg sync.WaitGroup

	for _, dork := range HIGH_VALUE_DORKS {
		for page := 0; page < 10; page++ {
			wg.Add(1)
			go func(q string, p int) {
				defer wg.Done()

				if atomic.LoadInt64(&progress.SBRequestsUsed) >= 1000 {
					return
				}
				atomic.AddInt64(&progress.SBRequestsUsed, 1)

				sem <- struct{}{}
				defer func() { <-sem }()

				params := url.Values{
					"api_key":       {SCRAPINGBEE_KEY},
					"google_query":  {q},
					"num":           {"100"},
					"page":          {strconv.Itoa(p)},
					"premium_proxy": {"true"},
					"json_response": {"true"},
				}

				apiURL := "https://app.scrapingbee.com/api/v1/store/google?" + params.Encode()
				resp, err := httpClient.Get(apiURL)
				if err != nil {
					log.Printf("[SB] Error dork=%s page=%d: %v", q[:min(30, len(q))], p, err)
					return
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)

				if resp.StatusCode != 200 {
					log.Printf("[SB] Status %d dork=%s page=%d body=%s", resp.StatusCode, q[:min(30, len(q))], p, string(body)[:min(200, len(body))])
					return
				}

				// FIXED: Parse ScrapingBee JSON response correctly
				var data map[string]interface{}
				if err := json.Unmarshal(body, &data); err != nil {
					log.Printf("[SB] JSON parse error: %v", err)
					return
				}

				count := 0
				// Try multiple possible response structures
				extractLinks := func(items []interface{}) {
					for _, item := range items {
						m, ok := item.(map[string]interface{})
						if !ok {
							continue
						}
						link := ""
						if l, ok := m["link"].(string); ok {
							link = l
						} else if l, ok := m["url"].(string); ok {
							link = l
						} else if l, ok := m["href"].(string); ok {
							link = l
						}
						if link != "" {
							mu.Lock()
							results = append(results, link)
							mu.Unlock()
							count++
						}
					}
				}

				// Structure 1: organic_results array
				if orgResults, ok := data["organic_results"].([]interface{}); ok {
					extractLinks(orgResults)
				}
				// Structure 2: results array
				if res, ok := data["results"].([]interface{}); ok {
					extractLinks(res)
				}
				// Structure 3: nested in google_results
				if gr, ok := data["google_results"].(map[string]interface{}); ok {
					if org, ok := gr["organic_results"].([]interface{}); ok {
						extractLinks(org)
					}
				}
				// Structure 4: raw HTML fallback - extract URLs directly
				if count == 0 {
					if html, ok := data["html"].(string); ok {
						reLink := regexp.MustCompile(`href="(https?://[^"]*myshopify\.com[^"]*)"`)
						matches := reLink.FindAllStringSubmatch(html, -1)
						for _, m := range matches {
							mu.Lock()
							results = append(results, m[1])
							mu.Unlock()
							count++
						}
						reLink2 := regexp.MustCompile(`"(https?://[a-zA-Z0-9-]+\.myshopify\.com[^"]*)"`)
						matches2 := reLink2.FindAllStringSubmatch(html, -1)
						for _, m := range matches2 {
							mu.Lock()
							results = append(results, m[1])
							mu.Unlock()
							count++
						}
					}
				}

				atomic.AddInt64(&progress.Scraped, int64(count))
				if count > 0 {
					log.Printf("[SB] Got %d results from dork=%s page=%d", count, q[:min(30, len(q))], p)
				}
			}(dork, page)
		}
	}
	wg.Wait()
	log.Printf("[SCRAPE] Google dorks total: %d candidates", len(results))
	return results
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ==========================================
// SOURCE 2: MASSIVE DNS ENUMERATION
// ==========================================

func generateDNSWordlist() []string {
	prefixes := []string{
		"store", "shop", "buy", "deal", "sale", "offer", "best", "top", "new", "hot",
		"fashion", "style", "wear", "gear", "tech", "gadget", "home", "life", "fit",
		"sport", "outdoor", "beauty", "skin", "hair", "pet", "baby", "kid", "toy",
		"game", "book", "art", "craft", "food", "drink", "coffee", "tea", "wine",
		"health", "wellness", "yoga", "gym", "run", "bike", "swim", "camp", "hike",
		"luxury", "premium", "elite", "pro", "max", "ultra", "mega", "super", "hyper",
		"eco", "green", "organic", "natural", "pure", "fresh", "clean", "smart",
		"alpha", "beta", "gamma", "delta", "omega", "nova", "apex", "peak", "rise",
		"bolt", "flash", "swift", "rapid", "turbo", "nitro", "spark", "blaze", "flame",
		"ocean", "river", "lake", "mountain", "forest", "valley", "desert", "arctic",
		"urban", "metro", "city", "town", "village", "country", "world", "global",
		"daily", "weekly", "monthly", "seasonal", "annual", "forever", "eternal",
		"golden", "silver", "diamond", "crystal", "royal", "king", "queen", "crown",
		"red", "blue", "black", "white", "gold", "dark", "light", "bright", "vivid",
		"zen", "flow", "pulse", "wave", "drift", "shift", "lift", "glow", "bloom",
		"nest", "den", "hub", "lab", "box", "spot", "zone", "base", "core", "edge",
		"mint", "sage", "rose", "lily", "iris", "jade", "coral", "amber", "onyx",
		"fox", "wolf", "bear", "hawk", "eagle", "lion", "tiger", "panda", "koala",
		"sun", "moon", "star", "sky", "cloud", "rain", "snow", "storm", "thunder",
		"north", "south", "east", "west", "polar", "tropic", "equator", "horizon",
		"vintage", "retro", "classic", "modern", "future", "neo", "prime", "first",
		"happy", "lucky", "brave", "bold", "wild", "free", "true", "real", "epic",
		"silk", "linen", "cotton", "wool", "leather", "denim", "canvas", "velvet",
		"kitchen", "garden", "closet", "wardrobe", "pantry", "cellar", "attic",
		"market", "bazaar", "emporium", "boutique", "outlet", "depot", "supply",
	}

	suffixes := []string{
		"", "co", "hq", "us", "uk", "ca", "au", "de", "fr", "jp", "shop", "store",
		"official", "original", "direct", "online", "digital", "global", "world",
		"plus", "now", "go", "one", "two", "three", "x", "z", "io", "app",
	}

	var words []string
	for _, p := range prefixes {
		for _, s := range suffixes {
			if s == "" {
				words = append(words, p)
			} else {
				words = append(words, p+s)
				words = append(words, p+"-"+s)
				words = append(words, p+"_"+s)
			}
		}
	}

	// Add numeric variants for top prefixes
	topPrefixes := prefixes[:50]
	for _, p := range topPrefixes {
		for i := 1; i <= 99; i++ {
			words = append(words, fmt.Sprintf("%s%d", p, i))
		}
	}

	log.Printf("[DNS] Generated %d wordlist entries", len(words))
	return words
}

func enumerateDNS() []string {
	progress.Phase = "DNS Enumeration"
	wordlist := generateDNSWordlist()

	var results []string
	var mu sync.Mutex
	sem := make(chan struct{}, 1000)
	var wg sync.WaitGroup

	resolver := &net.Resolver{PreferGo: true}

	for _, word := range wordlist {
		domain := word + ".myshopify.com"
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			addrs, err := resolver.LookupHost(ctx, d)
			if err == nil && len(addrs) > 0 {
				mu.Lock()
				results = append(results, "https://"+d)
				mu.Unlock()
			}
			atomic.AddInt64(&progress.Scraped, 1)
		}(domain)
	}

	wg.Wait()
	log.Printf("[DNS] Found %d live subdomains from %d words", len(results), len(wordlist))
	return results
}

// ==========================================
// SOURCE 3: COMMONCRAWL INDEX FILTERING
// ==========================================

func scrapeCommonCrawlIndex() []string {
	progress.Phase = "CommonCrawl Index"

	// Latest CommonCrawl index endpoints
	indexURLs := []string{
		"https://index.commoncrawl.org/CC-MAIN-2024-51-index?url=*.myshopify.com&output=json&limit=100000",
		"https://index.commoncrawl.org/CC-MAIN-2024-46-index?url=*.myshopify.com&output=json&limit=100000",
		"https://index.commoncrawl.org/CC-MAIN-2024-42-index?url=*.myshopify.com&output=json&limit=100000",
	}

	var mu sync.Mutex
	var results []string
	var wg sync.WaitGroup

	for _, idxURL := range indexURLs {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			resp, err := httpClient.Get(u)
			if err != nil {
				log.Printf("[CC] Failed %s: %v", u, err)
				return
			}
			defer resp.Body.Close()

			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
			count := 0
			for scanner.Scan() {
				line := scanner.Text()
				var entry map[string]interface{}
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					continue
				}
				urlStr, ok := entry["url"].(string)
				if !ok || urlStr == "" {
					continue
				}
				// Extract domain from URL
				parsed, err := url.Parse(urlStr)
				if err != nil {
					continue
				}
				host := parsed.Hostname()
				if strings.HasSuffix(host, ".myshopify.com") {
					mu.Lock()
					results = append(results, "https://"+host)
					mu.Unlock()
					count++
				}
			}
			atomic.AddInt64(&progress.Scraped, int64(count))
			log.Printf("[CC] Got %d domains from %s", count, u[len(u)-30:])
		}(idxURL)
	}

	wg.Wait()
	log.Printf("[CC] Total CommonCrawl domains: %d", len(results))
	return results
}

// ==========================================
// VALIDATION ENGINE
// ==========================================

const STOREFRONT_QUERY = `query FetchProducts($first:Int!){products(first:$first){edges{node{handle variants(first:50){edges{node{id availableForSale requiresShipping price{amount currencyCode}}}}}}}}}`

var API_VERSIONS = []string{"2025-01", "2024-10", "2024-07", "2024-01"}

type ValidSite struct {
	Domain    string
	VariantID string
	Price     float64
	Currency  string
}

func fastExtract(data []byte, key string) []byte {
	search := []byte(`"` + key + `":`)
	idx := bytes.Index(data, search)
	if idx == -1 {
		return nil
	}
	start := idx + len(search)
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\n' || data[start] == '\r') {
		start++
	}
	if start >= len(data) {
		return nil
	}
	if data[start] == '"' {
		end := start + 1
		for end < len(data) {
			if data[end] == '\\' {
				end += 2
				continue
			}
			if data[end] == '"' {
				return data[start+1 : end]
			}
			end++
		}
	} else if data[start] == '{' || data[start] == '[' {
		bracket := data[start]
		closeBracket := byte('}')
		if bracket == '[' {
			closeBracket = ']'
		}
		depth := 1
		end := start + 1
		for end < len(data) && depth > 0 {
			if data[end] == '"' {
				end++
				for end < len(data) && data[end] != '"' {
					if data[end] == '\\' {
						end++
					}
					end++
				}
			} else if data[end] == bracket {
				depth++
			} else if data[end] == closeBracket {
				depth--
			}
			end++
		}
		return data[start:end]
	} else {
		end := start
		for end < len(data) && data[end] != ',' && data[end] != '}' && data[end] != ']' {
			end++
		}
		return bytes.TrimSpace(data[start:end])
	}
	return nil
}

func fstr(data []byte, key string) string { return string(fastExtract(data, key)) }

func validateSite(ctx context.Context, rawURL string) *ValidSite {
	baseURL := strings.TrimSpace(rawURL)
	if baseURL == "" || strings.HasPrefix(baseURL, "#") {
		return nil
	}
	if !strings.HasPrefix(baseURL, "http") {
		baseURL = "https://" + baseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 5*1024))
	htmlText := string(bodyBytes)
	lower := strings.ToLower(htmlText)

	if strings.Contains(lower, "password page") || (strings.Contains(lower, "password") && strings.Contains(lower, "enter")) {
		return nil
	}
	if !strings.Contains(lower, "cdn.shopify.com") && !strings.Contains(lower, "shopify.theme") && !strings.Contains(lower, "myshopify.com") {
		return nil
	}

	token := ""
	reToken := regexp.MustCompile(`storefrontAccessToken["']?\s*[:=]\s*["']([a-f0-9]{32})`)
	if m := reToken.FindStringSubmatch(htmlText); len(m) > 1 {
		token = m[1]
	}

	var gqlData map[string]interface{}
	for _, ver := range API_VERSIONS {
		gqlURL := fmt.Sprintf("%s/api/%s/graphql.json", baseURL, ver)
		payload, _ := json.Marshal(map[string]interface{}{
			"query": STOREFRONT_QUERY, "variables": map[string]int{"first": 50},
		})
		gqlReq, _ := http.NewRequestWithContext(ctx, "POST", gqlURL, bytes.NewReader(payload))
		gqlReq.Header.Set("Content-Type", "application/json")
		if token != "" {
			gqlReq.Header.Set("X-Shopify-Storefront-Access-Token", token)
		}
		gqlResp, err := httpClient.Do(gqlReq)
		if err != nil {
			continue
		}
		gqlBody, _ := io.ReadAll(gqlResp.Body)
		gqlResp.Body.Close()
		if gqlResp.StatusCode == 200 {
			if err := json.Unmarshal(gqlBody, &gqlData); err == nil && gqlData["data"] != nil {
				break
			}
		}
	}

	if gqlData == nil || gqlData["data"] == nil {
		return nil
	}

	productsData, ok := gqlData["data"].(map[string]interface{})["products"].(map[string]interface{})
	if !ok {
		return nil
	}
	edges, ok := productsData["edges"].([]interface{})
	if !ok || len(edges) == 0 {
		return nil
	}

	type candidate struct {
		id       string
		price    float64
		currency string
		band     int
		shipping bool
	}
	var candidates []candidate
	reVid := regexp.MustCompile(`ProductVariant/(\d+)`)

	for _, edge := range edges {
		node := edge.(map[string]interface{})["node"].(map[string]interface{})
		variants := node["variants"].(map[string]interface{})["edges"].([]interface{})
		for _, vEdge := range variants {
			vNode := vEdge.(map[string]interface{})["node"].(map[string]interface{})
			avail, _ := vNode["availableForSale"].(bool)
			if !avail {
				continue
			}
			idStr, _ := vNode["id"].(string)
			m := reVid.FindStringSubmatch(idStr)
			if len(m) < 2 {
				continue
			}
			priceMap, _ := vNode["price"].(map[string]interface{})
			amtStr, _ := priceMap["amount"].(string)
			curStr, _ := priceMap["currencyCode"].(string)
			price, err := strconv.ParseFloat(amtStr, 64)
			if err != nil || price <= 0 {
				continue
			}
			ship, _ := vNode["requiresShipping"].(bool)
			band := 4
			if price <= 1.0 {
				band = 1
			} else if price <= 5.0 {
				band = 2
			} else if price <= 10.0 {
				band = 3
			}
			candidates = append(candidates, candidate{id: m[1], price: price, currency: curStr, band: band, shipping: ship})
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.band < best.band ||
			(c.band == best.band && c.shipping && !best.shipping) ||
			(c.band == best.band && c.shipping == best.shipping && c.price < best.price) {
			best = c
		}
	}
	return &ValidSite{Domain: baseURL, VariantID: best.id, Price: best.price, Currency: best.currency}
}

// ==========================================
// MAIN PIPELINE
// ==========================================

func main() {
	rand.Seed(time.Now().UnixNano())
	log.Println("🚀 Shopify Harvester starting...")

	messageID := startProgressUpdater()

	// Stage 1: Harvest from ALL sources concurrently
	var allCandidates []string
	var harvestMu sync.Mutex
	var harvestWg sync.WaitGroup

	appendResults := func(name string, results []string) {
		harvestMu.Lock()
		allCandidates = append(allCandidates, results...)
		harvestMu.Unlock()
		log.Printf("📦 %s contributed %d candidates", name, len(results))
	}

	// Run all sources concurrently
	harvestWg.Add(3)
	go func() {
		defer harvestWg.Done()
		r := scrapeGoogleDorks()
		appendResults("Google Dorks", r)
	}()
	go func() {
		defer harvestWg.Done()
		r := enumerateDNS()
		appendResults("DNS Enumeration", r)
	}()
	go func() {
		defer harvestWg.Done()
		r := scrapeCommonCrawlIndex()
		appendResults("CommonCrawl", r)
	}()

	harvestWg.Wait()

	// Deduplicate
	seen := make(map[string]bool)
	var unique []string
	for _, u := range allCandidates {
		norm := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u), "/"))
		if norm != "" && !seen[norm] {
			seen[norm] = true
			unique = append(unique, u)
		}
	}
	atomic.StoreInt64(&progress.TotalCandidates, int64(len(unique)))
	log.Printf("📊 Total unique candidates: %d", len(unique))

	// Stage 2: Validate
	progress.Phase = "Validation"
	log.Println("🔍 Validating sites...")

	var validSites []ValidSite
	var mu sync.Mutex
	sem := make(chan struct{}, WORKERS)
	var wg sync.WaitGroup

	for _, site := range unique {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			atomic.AddInt64(&progress.Validated, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result := validateSite(ctx, s)
			if result != nil {
				mu.Lock()
				validSites = append(validSites, *result)
				mu.Unlock()
				atomic.AddInt64(&progress.ValidFound, 1)
			}
		}(site)
	}
	wg.Wait()

	// Stage 3: Output
	stopProgressUpdater(messageID)

	var output strings.Builder
	for _, vs := range validSites {
		output.WriteString(fmt.Sprintf("%s|%s|%.2f|%s\n", vs.Domain, vs.VariantID, vs.Price, vs.Currency))
	}
	os.WriteFile("valid_sites.txt", []byte(output.String()), 0644)
	fileInfo, _ := os.Stat("valid_sites.txt")
	log.Printf("✅ %d valid sites saved (%.2f MB)", len(validSites), float64(fileInfo.Size())/(1024*1024))

	splitAndSendFiles("valid_sites.txt")
	log.Println("🎉 Harvest complete!")
}
