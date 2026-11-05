package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ticker struct {
	Exchange     string   `json:"exchange"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Chain        string   `json:"chain,omitempty"`
	ChainName    string   `json:"chain_name,omitempty"`
	Market       string   `json:"market"`
	Quote        string   `json:"quote"`
	Price        float64  `json:"price"`
	Bid          float64  `json:"bid"`
	Ask          float64  `json:"ask"`
	Change24h    *float64 `json:"change_24h"`
	VolumeQuote  float64  `json:"volume_24h_quote"`
	LiquidityUSD float64  `json:"liquidity_usd,omitempty"`
	LowLiquidity bool     `json:"low_liquidity,omitempty"`
	Pool         string   `json:"pool,omitempty"`
	// Suspect means the venue's print failed the same-token USD-pool
	// cross-check: shown (so the data defect is visible), never alerted on.
	Suspect     bool    `json:"suspect,omitempty"`
	SuspectNote string  `json:"suspect_note,omitempty"`
	PriceUSDT   float64 `json:"price_usdt"`
	BidUSDT     float64 `json:"bid_usdt"`
	AskUSDT     float64 `json:"ask_usdt"`
	LatencyMS   int64   `json:"latency_ms"`
	FetchedAt   int64   `json:"fetched_at"`
	Error       string  `json:"error,omitempty"`
	NotListed   bool    `json:"not_listed,omitempty"`
}

type leg struct {
	Exchange string  `json:"exchange"`
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
}

type summary struct {
	MedianPrice     float64 `json:"median_price"`
	MinPrice        float64 `json:"min_price"`
	MaxPrice        float64 `json:"max_price"`
	DispersionBps   float64 `json:"dispersion_bps"`
	BestBid         *leg    `json:"best_bid"`
	BestAsk         *leg    `json:"best_ask"`
	CrossSpreadBps  float64 `json:"cross_spread_bps"`
	CrossSpreadUSDT float64 `json:"cross_spread_usdt"`
	OkCount         int     `json:"ok_count"`
	TotalCount      int     `json:"total_count"`
}

type comparison struct {
	Base       string   `json:"base"`
	Quote      string   `json:"quote"`
	FetchedAt  int64    `json:"fetched_at"`
	ElapsedMS  int64    `json:"elapsed_ms"`
	USDTUSD    float64  `json:"usdt_usd"`
	USDTSource string   `json:"usdt_usd_source"`
	Quotes     []ticker `json:"quotes"`
	Summary    summary  `json:"summary"`
}

var symbolCatalog = []struct {
	Base string
	Name string
}{
	{"BTC", "Bitcoin"}, {"ETH", "Ethereum"}, {"SOL", "Solana"}, {"BNB", "BNB"},
	{"XRP", "XRP"}, {"DOGE", "Dogecoin"}, {"ADA", "Cardano"}, {"TRX", "TRON"},
	{"AVAX", "Avalanche"}, {"LINK", "Chainlink"}, {"DOT", "Polkadot"}, {"SUI", "Sui"}, {"LTC", "Litecoin"}, {"BCH", "Bitcoin Cash"}, {"NEAR", "NEAR"},
	{"APT", "Aptos"}, {"ARB", "Arbitrum"}, {"OP", "Optimism"}, {"PEPE", "Pepe"},
	{"SHIB", "Shiba Inu"}, {"XLM", "Stellar"}, {"HBAR", "Hedera"}, {"ATOM", "Cosmos"},
	{"FIL", "Filecoin"}, {"INJ", "Injective"}, {"UNI", "Uniswap"}, {"AAVE", "Aave"},
	{"ETC", "Ethereum Classic"}, {"WLD", "Worldcoin"},
}

var baseRe = regexp.MustCompile(`^[A-Z0-9]{2,12}$`)

var (
	httpClient = &http.Client{Timeout: 5 * time.Second}

	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}

	flightMu sync.Mutex
	flights  = map[string]*flight{}
)

const cacheTTL = 2 * time.Second

type cacheEntry struct {
	at   time.Time
	body []byte
}

type flight struct {
	done chan struct{}
	body []byte
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8990", "listen address")
	static := flag.String("static", "static", "optional static dir served on / (nginx serves it in production)")
	interval := flag.Duration("interval", 60*time.Second, "detector sampling interval (0 disables the monitor)")
	warmInterval := flag.Duration("warm-interval", 20*time.Second, "sampling interval while baselines are warming up")
	stateDir := flag.String("state", "/var/lib/price-compare", "state directory for events and baseline snapshots")
	minSamples := flag.Int("min-samples", 15, "samples required before a baseline is trusted")
	testAlert := flag.Bool("test-alert", false, "send one test alert through the configured notifier and exit")
	flag.Parse()

	if *testAlert {
		n := newNotifier()
		log.Printf("test-alert: notifier=%s", n.Name())
		n.Notify(Event{
			At: time.Now().UnixMilli(), Kind: kindVenueDepeg, Symbol: "TEST",
			Venue: "binance", VenueName: "Binance", MarketKind: "cex",
			Price: 100, Consensus: 102, DevBps: -196.1, BaselineBps: 2.4,
			ThresholdBps: 45, Severity: "alert", Samples: 42,
		})
		// give the async sender time to finish before the process exits
		time.Sleep(3 * time.Second)
		return
	}

	ads := adapters()
	dexes := dexAdapters()
	names := make([]string, 0, len(ads))
	for _, a := range ads {
		names = append(names, a.Name)
	}
	log.Printf("price-compare listening on %s, %d venues (%d cex + %d dex): %s", *addr, len(ads), len(ads)-len(dexes), len(dexes), strings.Join(names, ", "))

	// The monitor is the part that can detect an event: it samples every
	// catalog symbol on a timer and keeps a rolling baseline per venue.
	var det *detector
	var col *collector
	if *interval > 0 {
		if err := os.MkdirAll(*stateDir, 0o755); err != nil {
			log.Printf("monitor: state dir %s unusable: %v", *stateDir, err)
		}
		cfg := defaultDetectConfig()
		if *minSamples > 0 {
			cfg.MinSamples = *minSamples
		}
		applyDetectEnv(&cfg)
		log.Printf("monitor: thresholds cex=%.0fbps dex=%.0fbps peg=%.0fbps confirm=%d thin_cex_volume=%.0f$ thin_cex_floor=%.0fbps",
			cfg.CEXFloorBps, cfg.DEXFloorBps, cfg.PegFloorBps, cfg.Confirm, cfg.ThinCEXVolumeUSD, cfg.ThinCEXFloorBps)
		det = newDetector(cfg,
			filepath.Join(*stateDir, "events.jsonl"),
			filepath.Join(*stateDir, "baseline.json"),
			newNotifier())
		col = newCollector(det, *interval, *warmInterval)
		go col.Run(context.Background())
		log.Printf("monitor: %d symbols every %s (warm-up %s, alerts=%s)",
			len(symbolCatalog), *interval, *warmInterval, det.Status(0).Notifier)
	} else {
		log.Printf("monitor: disabled (-interval 0)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/healthz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"ok":        true,
			"service":   "price-compare",
			"exchanges": len(ads),
			"dex":       len(dexes),
			"time":      time.Now().UnixMilli(),
			"monitor":   det != nil,
		}
		if col != nil {
			body["cycles"] = col.Status().Cycles
		}
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if col == nil {
			writeJSON(w, http.StatusOK, monitorStatus{Enabled: false})
			return
		}
		writeJSON(w, http.StatusOK, col.Status())
	})

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if det == nil {
			writeJSON(w, http.StatusOK, map[string]any{"events": []Event{}, "enabled": false})
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		var since int64
		if v := r.URL.Query().Get("since"); v != "" {
			since, _ = strconv.ParseInt(v, 10, 64)
		}
		ev := det.Events(limit, since)
		writeJSON(w, http.StatusOK, map[string]any{"events": ev, "enabled": true, "count": len(ev)})
	})

	mux.HandleFunc("/api/signals", func(w http.ResponseWriter, r *http.Request) {
		if det == nil {
			writeJSON(w, http.StatusOK, map[string]any{"signals": []Signal{}, "enabled": false})
			return
		}
		base := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("base")))
		if base == "" {
			base = "BTC"
		}
		if !baseRe.MatchString(base) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base symbol"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"base":    base,
			"signals": det.Signals(base),
			"status":  col.Status(),
		})
	})
	mux.HandleFunc("/api/symbols", func(w http.ResponseWriter, r *http.Request) {
		type sym struct {
			Base string `json:"base"`
			Name string `json:"name"`
		}
		out := make([]sym, 0, len(symbolCatalog))
		for _, s := range symbolCatalog {
			out = append(out, sym{s.Base, s.Name})
		}
		writeJSON(w, http.StatusOK, map[string]any{"symbols": out, "exchanges": len(ads), "dex_exchanges": len(dexes)})
	})
	mux.HandleFunc("/api/compare", func(w http.ResponseWriter, r *http.Request) {
		base := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("base")))
		if base == "" {
			base = "BTC"
		}
		if !baseRe.MatchString(base) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base symbol"})
			return
		}
		body, err := compareCached(base)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(body)
	})

	if *static != "" {
		if _, err := os.Stat(*static); err == nil {
			mux.Handle("/", http.FileServer(http.Dir(*static)))
		}
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// compareCached coalesces concurrent requests for the same base and serves a
// 2s snapshot, so a page on auto-refresh cannot hammer the exchanges.
func compareCached(base string) ([]byte, error) {
	cacheMu.Lock()
	if e, ok := cache[base]; ok && time.Since(e.at) < cacheTTL {
		cacheMu.Unlock()
		return e.body, nil
	}
	cacheMu.Unlock()

	flightMu.Lock()
	if fl, ok := flights[base]; ok {
		flightMu.Unlock()
		<-fl.done
		return fl.body, nil
	}
	fl := &flight{done: make(chan struct{})}
	flights[base] = fl
	flightMu.Unlock()

	body, err := buildComparison(base)
	if err == nil {
		cacheMu.Lock()
		cache[base] = cacheEntry{at: time.Now(), body: body}
		cacheMu.Unlock()
	}

	flightMu.Lock()
	delete(flights, base)
	flightMu.Unlock()
	fl.body = body
	close(fl.done)
	return body, err
}

func buildComparison(base string) ([]byte, error) {
	c := buildComparisonValue(base)
	return json.Marshal(c)
}

// buildComparisonValue is the single fetch path shared by the page (through
// the 2s cache) and the background collector (which never uses that cache).
func buildComparisonValue(base string) comparison {
	start := time.Now()
	ads := adapters()
	rate, rateSource := usdtUSD()

	quotes := make([]ticker, len(ads))
	var wg sync.WaitGroup
	for i, a := range ads {
		wg.Add(1)
		go func(i int, a adapter) {
			defer wg.Done()
			market := a.Market(base)
			displayMarket := market
			if a.Kind == "dex" {
				// The fetch key is a contract address; only the pool label from
				// the response is meaningful to a reader.
				displayMarket = ""
			}
			t0 := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 4500*time.Millisecond)
			defer cancel()
			raw, err := a.Fetch(ctx, httpClient, market)
			q := ticker{
				Exchange:  a.ID,
				Name:      a.Name,
				Kind:      kindOf(a),
				Chain:     a.Chain,
				ChainName: a.ChainName,
				Market:    displayMarket,
				Quote:     a.Quote,
				LatencyMS: time.Since(t0).Milliseconds(),
				FetchedAt: time.Now().UnixMilli(),
			}
			if err != nil {
				if err == errNotListed {
					q.NotListed = true
				} else {
					q.Error = err.Error()
				}
				quotes[i] = q
				return
			}
			conv := 1.0
			if a.Quote == "USD" {
				conv = 1 / rate // 1 USD is worth 1/rate USDT
			}
			q.Price, q.Bid, q.Ask = raw.Price, raw.Bid, raw.Ask
			q.Change24h = raw.Change24h
			q.VolumeQuote = raw.QuoteVolume
			q.LiquidityUSD = raw.LiquidityUSD
			q.Pool = raw.Pool
			q.Suspect = raw.Suspect
			q.SuspectNote = raw.SuspectNote
			// A pool thinner than this can be moved by a single small trade, so
			// its print is not a comparable venue price. It stays visible and
			// labelled, but is kept out of the median / spread maths.
			q.LowLiquidity = thinPool(a.Kind, raw.LiquidityUSD)
			if raw.MarketLabel != "" {
				q.Market = raw.MarketLabel
			}
			q.PriceUSDT = raw.Price * conv
			q.BidUSDT = raw.Bid * conv
			q.AskUSDT = raw.Ask * conv
			quotes[i] = q
		}(i, a)
	}
	wg.Wait()

	sort.SliceStable(quotes, func(i, j int) bool {
		ai, aj := quotes[i].PriceUSDT > 0, quotes[j].PriceUSDT > 0
		if ai != aj {
			return ai
		}
		return quotes[i].PriceUSDT > quotes[j].PriceUSDT
	})

	sum := summarise(quotes)
	c := comparison{
		Base:       base,
		Quote:      "USDT",
		FetchedAt:  time.Now().UnixMilli(),
		ElapsedMS:  time.Since(start).Milliseconds(),
		USDTUSD:    rate,
		USDTSource: rateSource,
		Quotes:     quotes,
		Summary:    sum,
	}
	return c
}

func kindOf(a adapter) string {
	if a.Kind == "" {
		return "cex"
	}
	return a.Kind
}

// dexThinLiquidityUSD is the floor below which a pool's print is not treated
// as a comparable venue price: a single small trade can move it by percent.
const dexThinLiquidityUSD = 250_000

// thinPool reports whether an on-chain quote is too shallow to treat as a
// comparable venue price. Centralised books have no liquidity figure here, so
// they are never affected.
func thinPool(kind string, liquidityUSD float64) bool {
	return kind == "dex" && liquidityUSD > 0 && liquidityUSD < dexThinLiquidityUSD
}

func summarise(quotes []ticker) summary {
	sum := summary{TotalCount: len(quotes)}
	var prices []float64
	var bestBid, bestAsk *ticker
	for i := range quotes {
		q := &quotes[i]
		if q.Error != "" || q.NotListed || q.LowLiquidity || q.Suspect || q.PriceUSDT <= 0 {
			continue
		}
		sum.OkCount++
		prices = append(prices, q.PriceUSDT)
		if q.BidUSDT > 0 && (bestBid == nil || q.BidUSDT > bestBid.BidUSDT) {
			bestBid = q
		}
		if q.AskUSDT > 0 && (bestAsk == nil || q.AskUSDT < bestAsk.AskUSDT) {
			bestAsk = q
		}
	}
	if len(prices) == 0 {
		return sum
	}
	sort.Float64s(prices)
	n := len(prices)
	if n%2 == 1 {
		sum.MedianPrice = prices[n/2]
	} else {
		sum.MedianPrice = (prices[n/2-1] + prices[n/2]) / 2
	}
	sum.MinPrice, sum.MaxPrice = prices[0], prices[n-1]
	if sum.MinPrice > 0 {
		sum.DispersionBps = (sum.MaxPrice/sum.MinPrice - 1) * 10000
	}
	if bestBid != nil {
		sum.BestBid = &leg{bestBid.Exchange, bestBid.Name, bestBid.BidUSDT}
	}
	if bestAsk != nil {
		sum.BestAsk = &leg{bestAsk.Exchange, bestAsk.Name, bestAsk.AskUSDT}
	}
	if bestBid != nil && bestAsk != nil {
		mid := (bestBid.BidUSDT + bestAsk.AskUSDT) / 2
		if mid > 0 {
			sum.CrossSpreadBps = (bestBid.BidUSDT - bestAsk.AskUSDT) / mid * 10000
		}
		sum.CrossSpreadUSDT = bestBid.BidUSDT - bestAsk.AskUSDT
	}
	return sum
}

var (
	rateMu     sync.Mutex
	rateValue  = 1.0
	rateAt     time.Time
	rateSource = "fallback"
)

// usdtUSD returns the USDT/USD rate used to express USD-quoted books in USDT.
// A failure is surfaced as source "fallback" with rate 1.0 rather than hidden.
func usdtUSD() (float64, string) {
	rateMu.Lock()
	defer rateMu.Unlock()
	if time.Since(rateAt) < 60*time.Second && rateAt != (time.Time{}) {
		return rateValue, rateSource
	}
	var r struct {
		Result map[string]struct {
			CL []string `json:"c"`
		} `json:"result"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := getJSON(ctx, httpClient, "https://api.kraken.com/0/public/Ticker?pair=USDTZUSD", &r); err == nil {
		for _, d := range r.Result {
			if len(d.CL) > 0 {
				if v := f(d.CL[0]); v > 0.9 && v < 1.1 {
					rateValue, rateSource, rateAt = v, "kraken", time.Now()
					return rateValue, rateSource
				}
			}
		}
	}
	rateValue, rateSource, rateAt = 1.0, "fallback", time.Now()
	return rateValue, rateSource
}
