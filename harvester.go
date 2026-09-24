package main

import (
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
	Timeout: 15 * time.Second,
}

// ==========================================
// TELEGRAM HELPERS
// ==========================================

func tgSendText(msg string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TG_BOT_TOKEN)
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    TG_CHAT_ID,
		"text":       msg,
		"parse_mode": "HTML",
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
		"chat_id":    TG_CHAT_ID,
		"message_id": messageID,
		"text":       msg,
		"parse_mode": "HTML",
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
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("TG upload failed (%d): %s", resp.StatusCode, string(respBody))
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
			tgSendText(fmt.Sprintf("❌ File upload failed: %v", err))
		} else {
			tgSendText("✅ File sent successfully!")
		}
		return
	}

	lines := strings.Split(string(data), "\n")
	var currentChunk strings.Builder
	partNum := 1
	totalParts := 0

	// Pre-calculate total parts
	for _, line := range lines {
		if currentChunk.Len()+len(line)+1 > maxBytes && currentChunk.Len() > 0 {
			totalParts++
			currentChunk.Reset()
		}
		currentChunk.WriteString(line + "\n")
	}
	if currentChunk.Len() > 0 {
		totalParts++
	}

	currentChunk.Reset()
	partNum = 1

	for _, line := range lines {
		if currentChunk.Len()+len(line)+1 > maxBytes && currentChunk.Len() > 0 {
			chunkPath := fmt.Sprintf("valid_sites_part%d.txt", partNum)
			os.WriteFile(chunkPath, []byte(currentChunk.String()), 0644)

			caption := fmt.Sprintf("📁 Part %d/%d (%.2f MB)", partNum, totalParts, float64(currentChunk.Len())/(1024*1024))
			if err := tgSendFile(chunkPath, caption); err != nil {
				tgSendText(fmt.Sprintf("❌ Part %d upload failed: %v", partNum, err))
			}
			os.Remove(chunkPath)
			partNum++
			currentChunk.Reset()
			time.Sleep(500 * time.Millisecond) // Rate limit between uploads
		}
		currentChunk.WriteString(line + "\n")
	}

	if currentChunk.Len() > 0 {
		chunkPath := fmt.Sprintf("valid_sites_part%d.txt", partNum)
		os.WriteFile(chunkPath, []byte(currentChunk.String()), 0644)
		caption := fmt.Sprintf("📁 Part %d/%d (%.2f MB)", partNum, totalParts, float64(currentChunk.Len())/(1024*1024))
		if err := tgSendFile(chunkPath, caption); err != nil {
			tgSendText(fmt.Sprintf("❌ Part %d upload failed: %v", partNum, err))
		}
		os.Remove(chunkPath)
	}

	tgSendText(fmt.Sprintf("✅ All %d parts sent!", totalParts))
}

// ==========================================
// LIVE PROGRESS UPDATER
// ==========================================

func startProgressUpdater() int {
	tgSendText("🚀 <b>Shopify Harvester Started</b>\n⏳ Initializing...")
	time.Sleep(1 * time.Second)

	// Send initial message and get message_id for editing
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TG_BOT_TOKEN)
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    TG_CHAT_ID,
		"text":       "⏳ Starting...",
		"parse_mode": "HTML",
	})
	resp, err := httpClient.Post(apiURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[PROGRESS] Failed to create progress message: %v", err)
		return 0
	}
	defer resp.Body.Close()

	var result struct {
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
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
			phase := progress.Phase

			rate := float64(0)
			if elapsed > 0 {
				rate = float64(validated) / elapsed
			}

			progressBar := ""
			if total > 0 {
				pct := float64(validated) / float64(total) * 100
				filled := int(pct / 5)
				if filled > 20 {
					filled = 20
				}
				progressBar = strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
				progressBar = fmt.Sprintf("[%s] %.1f%%", progressBar, pct)
			}

			msg := fmt.Sprintf(
				"<b>🔄 Shopify Harvester — Live</b>\n\n"+
					"📍 Phase: <code>%s</code>\n"+
					"%s\n\n"+
					"📡 Scraped: <b>%d</b>\n"+
					"🔍 Validated: <b>%d / %d</b>\n"+
					"✅ Valid Found: <b>%d</b>\n"+
					"⚡ Rate: <b>%.1f sites/sec</b>\n"+
					"⏱️ Elapsed: <b>%.0fs</b>",
				phase, progressBar, scraped, validated, total, valid, rate, elapsed,
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
			"✅ Valid Sites: <b>%d</b>\n"+
			"📊 Total Scanned: <b>%d</b>\n"+
			"⏱️ Total Time: <b>%.1fs</b>\n\n"+
			"<i>Sending file(s)...</i>",
		valid, total, elapsed,
	)

	if messageID > 0 {
		tgEditMessage(messageID, msg)
	} else {
		tgSendText(msg)
	}
}

// ==========================================
// SOURCE 1: SCRAPINGBEE GOOGLE DORKS
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
}

func scrapeGoogleDorks() []string {
	progress.Phase = "Google Dorks (ScrapingBee)"

	var mu sync.Mutex
	var results []string
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for _, dork := range HIGH_VALUE_DORKS {
		for page := 0; page < 10; page++ {
			wg.Add(1)
			go func(q string, p int) {
				defer wg.Done()
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
					return
				}
				defer resp.Body.Close()

				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != 200 {
					return
				}

				var data map[string]interface{}
				if err := json.Unmarshal(body, &data); err != nil {
					return
				}

				orgResults, ok := data["organic_results"].([]interface{})
				if !ok {
					return
				}

				mu.Lock()
				for _, r := range orgResults {
					rm, ok := r.(map[string]interface{})
					if !ok {
						continue
					}
					link, _ := rm["link"].(string)
					if link != "" {
						results = append(results, link)
					}
				}
				mu.Unlock()

				atomic.AddInt64(&progress.Scraped, int64(len(orgResults)))
			}(dork, page)
		}
	}

	wg.Wait()
	log.Printf("[SCRAPE] Google dorks: %d candidates", len(results))
	return results
}

// ==========================================
// SOURCE 2: DNS ENUMERATION
// ==========================================

var DNS_WORDLIST = []string{
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
}

func enumerateDNS() []string {
	progress.Phase = "DNS Enumeration"

	var results []string
	var mu sync.Mutex
	sem := make(chan struct{}, 500)
	var wg sync.WaitGroup

	for _, word := range DNS_WORDLIST {
		domain := word + ".myshopify.com"
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			addrs, err := net.DefaultResolver.LookupHost(ctx, d)
			if err == nil && len(addrs) > 0 {
				mu.Lock()
				results = append(results, "https://"+d)
				mu.Unlock()
			}
			atomic.AddInt64(&progress.Scraped, 1)
		}(domain)
	}

	wg.Wait()
	log.Printf("[DNS] Found %d live subdomains", len(results))
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
			"query":     STOREFRONT_QUERY,
			"variables": map[string]int{"first": 50},
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

			candidates = append(candidates, candidate{
				id: m[1], price: price, currency: curStr, band: band, shipping: ship,
			})
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

	return &ValidSite{
		Domain:    baseURL,
		VariantID: best.id,
		Price:     best.price,
		Currency:  best.currency,
	}
}

// ==========================================
// MAIN PIPELINE
// ==========================================

func main() {
	rand.Seed(time.Now().UnixNano())
	log.Println("🚀 Shopify Harvester starting...")

	// Start live progress updater
	messageID := startProgressUpdater()

	// Stage 1: Harvest
	var allCandidates []string

	log.Println("📡 Scraping Google Dorks via ScrapingBee...")
	dorkResults := scrapeGoogleDorks()
	allCandidates = append(allCandidates, dorkResults...)

	log.Println("📡 DNS Enumeration...")
	dnsResults := enumerateDNS()
	allCandidates = append(allCandidates, dnsResults...)

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
	log.Printf("📊 Unique candidates: %d", len(unique))

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

	// Send file(s) to Telegram with auto-splitting
	splitAndSendFiles("valid_sites.txt")

	log.Println("🎉 Harvest complete!")
}
