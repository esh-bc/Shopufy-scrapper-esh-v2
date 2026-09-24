package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ============================================================
// CONFIG
// ============================================================
const (
	SCRAPINGBEE_KEY    = "YOUR_SCRAPINGBEE_KEY"
	TELEGRAM_BOT_TOKEN = "YOUR_BOT_TOKEN"
	TELEGRAM_CHAT_ID   = "YOUR_CHAT_ID"

	VALIDATOR_WORKERS = 3000
	CDX_WORKERS       = 50
	CRT_WORKERS       = 20
	BEE_WORKERS       = 5
	MAX_BEE_CREDITS   = 880
	TIMEOUT_SEC       = 10
	PROGRESS_INTERVAL = 30 * time.Second
	OUTPUT_FILE       = "valid_stores.txt"
)

// ============================================================
// STATS
// ============================================================
var (
	discovered int64
	valid      int64
	invalid    int64
	beeCredits int64
	startTime  = time.Now()

	sourceStatus = struct {
		sync.RWMutex
		m map[string]string
	}{m: map[string]string{
		"CommonCrawl": "starting",
		"crt.sh":      "starting",
		"ScrapingBee": "starting",
	}}
)

func setSource(name, status string) {
	sourceStatus.Lock()
	sourceStatus.m[name] = status
	sourceStatus.Unlock()
}

func getSourceMap() map[string]string {
	sourceStatus.RLock()
	defer sourceStatus.RUnlock()
	cp := make(map[string]string)
	for k, v := range sourceStatus.m {
		cp[k] = v
	}
	return cp
}

// ============================================================
// HTTP CLIENT
// ============================================================
var httpClient = &http.Client{
	Timeout: TIMEOUT_SEC * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        10000,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     30 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

// ============================================================
// TELEGRAM
// ============================================================
func tgSend(msg string) {
	if TELEGRAM_BOT_TOKEN == "YOUR_BOT_TOKEN" {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"chat_id":    TELEGRAM_CHAT_ID,
		"text":       msg,
		"parse_mode": "HTML",
	})
	resp, err := http.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TELEGRAM_BOT_TOKEN),
		"application/json",
		bytes.NewReader(payload),
	)
	if err == nil {
		resp.Body.Close()
	}
}

func tgSendFile(filePath string) {
	if TELEGRAM_BOT_TOKEN == "YOUR_BOT_TOKEN" {
		return
	}
	f, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", TELEGRAM_CHAT_ID)
	fw, err := w.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return
	}
	io.Copy(fw, f)
	w.Close()

	resp, err := http.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", TELEGRAM_BOT_TOKEN),
		w.FormDataContentType(),
		&buf,
	)
	if err == nil {
		resp.Body.Close()
	}
}

func progressMessage() string {
	sm := getSourceMap()
	elapsed := time.Since(startTime).Round(time.Second)
	d := atomic.LoadInt64(&discovered)
	v := atomic.LoadInt64(&valid)
	inv := atomic.LoadInt64(&invalid)
	credits := atomic.LoadInt64(&beeCredits)

	return fmt.Sprintf(
		"🔍 <b>Shopify Scraper Progress</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━━━━━\n"+
			"📡 <b>Sources:</b>\n"+
			"  CommonCrawl : %s\n"+
			"  crt.sh      : %s\n"+
			"  ScrapingBee : %s (%d/%d credits)\n\n"+
			"🌐 Discovered  : <b>%d</b>\n"+
			"✅ Valid stores : <b>%d</b>\n"+
			"❌ Invalid      : <b>%d</b>\n"+
			"⏱ Elapsed      : %s\n"+
			"━━━━━━━━━━━━━━━━━━━━━━━━",
		sm["CommonCrawl"], sm["crt.sh"], sm["ScrapingBee"],
		credits, MAX_BEE_CREDITS,
		d, v, inv, elapsed,
	)
}

func startProgressReporter(stop <-chan struct{}) {
	ticker := time.NewTicker(PROGRESS_INTERVAL)
	go func() {
		for {
			select {
			case <-ticker.C:
				tgSend(progressMessage())
			case <-stop:
				ticker.Stop()
				return
			}
		}
	}()
}

// ============================================================
// DEDUP
// ============================================================
var seen sync.Map

func addIfNew(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || len(domain) < 4 {
		return false
	}
	_, loaded := seen.LoadOrStore(domain, struct{}{})
	return !loaded
}

// ============================================================
// EXTRACT DOMAIN
// ============================================================
func extractDomain(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "http") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	if host == "" || !strings.Contains(host, ".") {
		return ""
	}
	return host
}

// ============================================================
// VALIDATOR — hits /products.json?limit=1
// ============================================================
func isValidShopify(domain string) bool {
	for _, scheme := range []string{"https", "http"} {
		u := fmt.Sprintf("%s://%s/products.json?limit=1", scheme, domain)
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; shopify-research-bot/1.0)")

		resp, err := httpClient.Do(req)
		if err != nil {
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			continue
		}

		var pr struct {
			Products []struct {
				ID int64 `json:"id"`
			} `json:"products"`
		}
		if json.Unmarshal(body, &pr) == nil && len(pr.Products) > 0 {
			return true
		}
	}
	return false
}

// ============================================================
// WRITER
// ============================================================
func startWriter(validCh <-chan string, done chan<- struct{}) {
	f, err := os.OpenFile(OUTPUT_FILE, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Println("writer error:", err)
		close(done)
		return
	}
	defer func() {
		f.Close()
		close(done)
	}()
	w := bufio.NewWriterSize(f, 64*1024)
	defer w.Flush()
	for domain := range validCh {
		_, _ = w.WriteString(domain + "\n")
	}
}

// ============================================================
// COMMON CRAWL
// ============================================================
func fetchCommonCrawl(domainCh chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	setSource("CommonCrawl", "🔄 running")

	indexes := []string{
		"CC-MAIN-2024-10",
		"CC-MAIN-2024-18",
		"CC-MAIN-2024-26",
		"CC-MAIN-2024-33",
		"CC-MAIN-2023-50",
		"CC-MAIN-2023-40",
	}
	queries := []string{
		"*.myshopify.com",
		"*/products.json",
	}

	sem := make(chan struct{}, CDX_WORKERS)
	var iwg sync.WaitGroup

	for _, idx := range indexes {
		for _, q := range queries {
			sem <- struct{}{}
			iwg.Add(1)
			go func(index, query string) {
				defer func() { <-sem; iwg.Done() }()

				apiURL := fmt.Sprintf(
					"https://index.commoncrawl.org/%s-index?url=%s&output=json&fl=url&limit=100000",
					index, url.QueryEscape(query),
				)
				resp, err := httpClient.Get(apiURL)
				if err != nil {
					return
				}
				defer resp.Body.Close()

				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)
				for scanner.Scan() {
					var result map[string]string
					if json.Unmarshal(scanner.Bytes(), &result) != nil {
						continue
					}
					if u, ok := result["url"]; ok {
						if d := extractDomain(u); d != "" && addIfNew(d) {
							atomic.AddInt64(&discovered, 1)
							domainCh <- d
						}
					}
				}
			}(idx, q)
		}
	}

	iwg.Wait()
	setSource("CommonCrawl", "✅ done")
}

// ============================================================
// CRT.SH
// ============================================================
func fetchCRT(domainCh chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	setSource("crt.sh", "🔄 running")

	apiURL := fmt.Sprintf("https://crt.sh/?q=%s&output=json",
		url.QueryEscape("%.myshopify.com"))

	resp, err := httpClient.Get(apiURL)
	if err != nil {
		setSource("crt.sh", "❌ error")
		return
	}
	defer resp.Body.Close()

	var results []struct {
		NameValue string `json:"name_value"`
	}
	body, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(body, &results) != nil {
		setSource("crt.sh", "❌ parse error")
		return
	}

	for _, r := range results {
		for _, line := range strings.Split(r.NameValue, "\n") {
			line = strings.TrimPrefix(strings.TrimSpace(line), "*.")
			if d := extractDomain(line); d != "" && addIfNew(d) {
				atomic.AddInt64(&discovered, 1)
				domainCh <- d
			}
		}
	}

	setSource("crt.sh", "✅ done")
}

// ============================================================
// SCRAPINGBEE — uses dedicated Google API endpoint
// Returns structured JSON, no HTML parsing needed
// Cost: 1 credit per request
// ============================================================

type BeeGoogleResponse struct {
	OrganicResults []struct {
		URL string `json:"url"`
	} `json:"organic_results"`
}

func fetchScrapingBee(domainCh chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	setSource("ScrapingBee", "🔄 running")

	if SCRAPINGBEE_KEY == "YOUR_SCRAPINGBEE_KEY" {
		setSource("ScrapingBee", "⚠️ no key")
		return
	}

	dorks := []string{
		`"cdn.shopify.com" -site:myshopify.com`,
		`"Powered by Shopify" -site:myshopify.com`,
		`"shopify-checkout-api-token" -site:myshopify.com`,
		`"window.Shopify" -site:myshopify.com`,
		`"/cdn/shop/" -site:myshopify.com`,
		`"shopify_pay" -site:myshopify.com`,
		`"shopify.com/s/files" -site:myshopify.com`,
		`"data-shopify" -site:myshopify.com`,
		`"shopify-features" -site:myshopify.com`,
		`"Shopify.theme" -site:myshopify.com`,
		`inurl:"/collections/all" "cdn.shopify.com"`,
		`"shopify_analytics" -site:myshopify.com`,
		`"myshopify" inurl:checkout -site:myshopify.com`,
		`intitle:"free shipping" "cdn.shopify.com"`,
		`"shopify_pay_integration" -site:myshopify.com`,
	}

	sem := make(chan struct{}, BEE_WORKERS)
	var bwg sync.WaitGroup

	for _, dork := range dorks {
		for page := 1; page <= 5; page++ {
			if atomic.LoadInt64(&beeCredits) >= MAX_BEE_CREDITS {
				goto done
			}

			sem <- struct{}{}
			bwg.Add(1)

			go func(d string, p int) {
				defer func() { <-sem; bwg.Done() }()

				// Use dedicated Google API endpoint — structured JSON response
				// Auth via Authorization header, not query param
				req, err := http.NewRequest("GET", "https://app.scrapingbee.com/api/v1/google", nil)
				if err != nil {
					return
				}

				q := req.URL.Query()
				q.Set("search", d)
				q.Set("nb_results", "10")
				q.Set("page", fmt.Sprintf("%d", p))
				q.Set("language", "en")
				req.URL.RawQuery = q.Encode()

				// Correct auth method from docs
				req.Header.Set("Authorization", "Bearer "+SCRAPINGBEE_KEY)

				resp, err := httpClient.Do(req)
				if err != nil {
					return
				}
				defer resp.Body.Close()
				atomic.AddInt64(&beeCredits, 1)

				body, _ := io.ReadAll(resp.Body)

				var result BeeGoogleResponse
				if json.Unmarshal(body, &result) != nil {
					return
				}

				// Structured results — no HTML parsing
				for _, item := range result.OrganicResults {
					if domain := extractDomain(item.URL); domain != "" && addIfNew(domain) {
						atomic.AddInt64(&discovered, 1)
						domainCh <- domain
					}
				}

				time.Sleep(200 * time.Millisecond)
			}(dork, page)
		}
	}

done:
	bwg.Wait()
	setSource("ScrapingBee", fmt.Sprintf("✅ done (%d credits used)", atomic.LoadInt64(&beeCredits)))
}

// ============================================================
// FINAL SUMMARY
// ============================================================
func sendFinalSummary(reason string) {
	elapsed := time.Since(startTime).Round(time.Second)
	msg := fmt.Sprintf(
		"🏁 <b>Scraper Finished</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━━━━━\n"+
			"Reason: %s\n\n"+
			"🌐 Total discovered : <b>%d</b>\n"+
			"✅ Valid stores     : <b>%d</b>\n"+
			"❌ Invalid          : <b>%d</b>\n"+
			"💳 Credits used     : <b>%d/%d</b>\n"+
			"⏱ Total time       : %s\n"+
			"━━━━━━━━━━━━━━━━━━━━━━━━\n"+
			"📄 Sending valid_stores.txt...",
		reason,
		atomic.LoadInt64(&discovered),
		atomic.LoadInt64(&valid),
		atomic.LoadInt64(&invalid),
		atomic.LoadInt64(&beeCredits), MAX_BEE_CREDITS,
		elapsed,
	)
	tgSend(msg)
	tgSendFile(OUTPUT_FILE)
}

// ============================================================
// MAIN
// ============================================================
func main() {
	fmt.Println("🚀 Shopify scraper starting...")
	tgSend("🚀 <b>Shopify Scraper Started</b>\n" +
		"Sources: CommonCrawl + crt.sh + ScrapingBee\n" +
		"Validators: 3000 goroutines\n" +
		"Output: valid_stores.txt")

	domainCh := make(chan string, 500000)
	validCh  := make(chan string, 10000)

	// OS signal handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Progress reporter
	progressStop := make(chan struct{})
	startProgressReporter(progressStop)

	// Writer goroutine
	writerDone := make(chan struct{})
	go startWriter(validCh, writerDone)

	// Validator pool
	var valWg sync.WaitGroup
	for i := 0; i < VALIDATOR_WORKERS; i++ {
		valWg.Add(1)
		go func() {
			defer valWg.Done()
			for domain := range domainCh {
				if isValidShopify(domain) {
					atomic.AddInt64(&valid, 1)
					validCh <- domain
				} else {
					atomic.AddInt64(&invalid, 1)
				}
			}
		}()
	}

	// Source fetchers
	var srcWg sync.WaitGroup
	srcWg.Add(3)
	go fetchCommonCrawl(domainCh, &srcWg)
	go fetchCRT(domainCh, &srcWg)
	go fetchScrapingBee(domainCh, &srcWg)

	// Wait for sources or signal
	allDone := make(chan struct{})
	go func() {
		srcWg.Wait()
		close(allDone)
	}()

	stopReason := ""
	select {
	case <-sigCh:
		stopReason = "Manual stop (SIGINT/SIGTERM)"
		fmt.Println("\n⚠️  Signal received — flushing and exiting...")
	case <-allDone:
		stopReason = "All sources exhausted"
		fmt.Println("✅ All sources done")
	}

	// Graceful shutdown
	close(progressStop)
	close(domainCh)

	fmt.Println("⏳ Waiting for validators to finish...")
	valWg.Wait()

	close(validCh)
	<-writerDone

	fmt.Printf("\n🏁 Done. Valid stores: %d\n", atomic.LoadInt64(&valid))
	sendFinalSummary(stopReason)
}
