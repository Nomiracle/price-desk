package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// errNotListed means the exchange answered correctly but does not trade this
// base against the quote currency. It is rendered as "未上架", never as an
// error, so a missing market cannot be mistaken for an outage.
var errNotListed = errors.New("market not listed")

type rawTicker struct {
	Price        float64
	Bid          float64
	Ask          float64
	Change24h    *float64 // percent, already multiplied by 100
	QuoteVolume  float64
	LiquidityUSD float64 // DEX only: pool liquidity, USD
	MarketLabel  string  // DEX only: "<base>/<quote>" of the pool actually priced
	Pool         string  // DEX only: pool address
}

type adapter struct {
	ID        string
	Name      string
	Kind      string // "cex" (empty is treated as cex) or "dex"
	Chain     string // dex only: source chain id
	ChainName string // dex only: human label for Chain
	Quote     string // settlement quote of the market we query
	Market    func(base string) string
	Fetch     func(ctx context.Context, c *http.Client, market string) (rawTicker, error)
}

func f(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func pct(v float64) *float64 { return &v }

func getRaw(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "price-compare/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		return nil, errNotListed
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	body, err := getRaw(ctx, c, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return err
	}
	return nil
}

// adapters returns every venue shown by the API: centralised books first, then
// on-chain pools (dex.go).
func adapters() []adapter {
	return append(cexAdapters(), dexAdapters()...)
}

func cexAdapters() []adapter {
	return []adapter{
		{
			ID: "binance", Name: "Binance", Quote: "USDT",
			Market: func(b string) string { return b + "USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					LastPrice          string `json:"lastPrice"`
					BidPrice           string `json:"bidPrice"`
					AskPrice           string `json:"askPrice"`
					PriceChangePercent string `json:"priceChangePercent"`
					QuoteVolume        string `json:"quoteVolume"`
				}
				if err := getJSON(ctx, c, "https://api.binance.com/api/v3/ticker/24hr?symbol="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if r.LastPrice == "" {
					return rawTicker{}, errNotListed
				}
				return rawTicker{Price: f(r.LastPrice), Bid: f(r.BidPrice), Ask: f(r.AskPrice), Change24h: pct(f(r.PriceChangePercent)), QuoteVolume: f(r.QuoteVolume)}, nil
			},
		},
		{
			ID: "okx", Name: "OKX", Quote: "USDT",
			Market: func(b string) string { return b + "-USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					Code string `json:"code"`
					Data []struct {
						Last      string `json:"last"`
						AskPx     string `json:"askPx"`
						BidPx     string `json:"bidPx"`
						Open24h   string `json:"open24h"`
						VolCcy24h string `json:"volCcy24h"`
					} `json:"data"`
				}
				if err := getJSON(ctx, c, "https://www.okx.com/api/v5/market/ticker?instId="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if len(r.Data) == 0 || r.Data[0].Last == "" {
					return rawTicker{}, errNotListed
				}
				d := r.Data[0]
				last, open := f(d.Last), f(d.Open24h)
				var ch *float64
				if open > 0 {
					ch = pct((last/open - 1) * 100)
				}
				return rawTicker{Price: last, Bid: f(d.BidPx), Ask: f(d.AskPx), Change24h: ch, QuoteVolume: f(d.VolCcy24h)}, nil
			},
		},
		{
			ID: "bybit", Name: "Bybit", Quote: "USDT",
			Market: func(b string) string { return b + "USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					RetCode int `json:"retCode"`
					Result  struct {
						List []struct {
							Bid1Price    string `json:"bid1Price"`
							Ask1Price    string `json:"ask1Price"`
							LastPrice    string `json:"lastPrice"`
							Price24hPcnt string `json:"price24hPcnt"`
							Turnover24h  string `json:"turnover24h"`
						} `json:"list"`
					} `json:"result"`
				}
				if err := getJSON(ctx, c, "https://api.bybit.com/v5/market/tickers?category=spot&symbol="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if r.RetCode != 0 || len(r.Result.List) == 0 || r.Result.List[0].LastPrice == "" {
					return rawTicker{}, errNotListed
				}
				d := r.Result.List[0]
				return rawTicker{Price: f(d.LastPrice), Bid: f(d.Bid1Price), Ask: f(d.Ask1Price), Change24h: pct(f(d.Price24hPcnt) * 100), QuoteVolume: f(d.Turnover24h)}, nil
			},
		},
		{
			ID: "bitget", Name: "Bitget", Quote: "USDT",
			Market: func(b string) string { return b + "USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					Code string `json:"code"`
					Data []struct {
						LastPr      string `json:"lastPr"`
						BidPr       string `json:"bidPr"`
						AskPr       string `json:"askPr"`
						Change24h   string `json:"change24h"`
						QuoteVolume string `json:"quoteVolume"`
					} `json:"data"`
				}
				if err := getJSON(ctx, c, "https://api.bitget.com/api/v2/spot/market/tickers?symbol="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if r.Code != "00000" || len(r.Data) == 0 || r.Data[0].LastPr == "" {
					return rawTicker{}, errNotListed
				}
				d := r.Data[0]
				return rawTicker{Price: f(d.LastPr), Bid: f(d.BidPr), Ask: f(d.AskPr), Change24h: pct(f(d.Change24h) * 100), QuoteVolume: f(d.QuoteVolume)}, nil
			},
		},
		{
			ID: "gate", Name: "Gate", Quote: "USDT",
			Market: func(b string) string { return b + "_USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r []struct {
					Last             string `json:"last"`
					LowestAsk        string `json:"lowest_ask"`
					HighestBid       string `json:"highest_bid"`
					ChangePercentage string `json:"change_percentage"`
					QuoteVolume      string `json:"quote_volume"`
				}
				if err := getJSON(ctx, c, "https://api.gateio.ws/api/v4/spot/tickers?currency_pair="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if len(r) == 0 || r[0].Last == "" {
					return rawTicker{}, errNotListed
				}
				return rawTicker{Price: f(r[0].Last), Bid: f(r[0].HighestBid), Ask: f(r[0].LowestAsk), Change24h: pct(f(r[0].ChangePercentage)), QuoteVolume: f(r[0].QuoteVolume)}, nil
			},
		},
		{
			ID: "kucoin", Name: "KuCoin", Quote: "USDT",
			Market: func(b string) string { return b + "-USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					Code string `json:"code"`
					Data struct {
						Last       string `json:"last"`
						Buy        string `json:"buy"`
						Sell       string `json:"sell"`
						ChangeRate string `json:"changeRate"`
						VolValue   string `json:"volValue"`
					} `json:"data"`
				}
				if err := getJSON(ctx, c, "https://api.kucoin.com/api/v1/market/stats?symbol="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if r.Code != "200000" || r.Data.Last == "" {
					return rawTicker{}, errNotListed
				}
				return rawTicker{Price: f(r.Data.Last), Bid: f(r.Data.Buy), Ask: f(r.Data.Sell), Change24h: pct(f(r.Data.ChangeRate) * 100), QuoteVolume: f(r.Data.VolValue)}, nil
			},
		},
		{
			ID: "mexc", Name: "MEXC", Quote: "USDT",
			Market: func(b string) string { return b + "USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					LastPrice          string `json:"lastPrice"`
					BidPrice           string `json:"bidPrice"`
					AskPrice           string `json:"askPrice"`
					PriceChangePercent string `json:"priceChangePercent"`
					QuoteVolume        string `json:"quoteVolume"`
				}
				if err := getJSON(ctx, c, "https://api.mexc.com/api/v3/ticker/24hr?symbol="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if r.LastPrice == "" {
					return rawTicker{}, errNotListed
				}
				// MEXC reports the 24h change as a ratio, unlike Binance's percent.
				return rawTicker{Price: f(r.LastPrice), Bid: f(r.BidPrice), Ask: f(r.AskPrice), Change24h: pct(f(r.PriceChangePercent) * 100), QuoteVolume: f(r.QuoteVolume)}, nil
			},
		},
		{
			ID: "htx", Name: "HTX", Quote: "USDT",
			Market: func(b string) string { return strings.ToLower(b) + "usdt" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					Status string `json:"status"`
					Tick   struct {
						Open  float64   `json:"open"`
						Close float64   `json:"close"`
						Bid   []float64 `json:"bid"`
						Ask   []float64 `json:"ask"`
						Vol   float64   `json:"vol"`
					} `json:"tick"`
				}
				if err := getJSON(ctx, c, "https://api.huobi.pro/market/detail/merged?symbol="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if r.Status != "ok" || r.Tick.Close == 0 {
					return rawTicker{}, errNotListed
				}
				var ch *float64
				if r.Tick.Open > 0 {
					ch = pct((r.Tick.Close/r.Tick.Open - 1) * 100)
				}
				var bid, ask float64
				if len(r.Tick.Bid) > 0 {
					bid = r.Tick.Bid[0]
				}
				if len(r.Tick.Ask) > 0 {
					ask = r.Tick.Ask[0]
				}
				return rawTicker{Price: r.Tick.Close, Bid: bid, Ask: ask, Change24h: ch, QuoteVolume: r.Tick.Vol}, nil
			},
		},
		{
			ID: "kraken", Name: "Kraken", Quote: "USDT",
			Market: func(b string) string {
				if b == "BTC" {
					return "XBTUSDT"
				}
				return b + "USDT"
			},
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					Error  []string `json:"error"`
					Result map[string]struct {
						Ask []string `json:"a"`
						Bid []string `json:"b"`
						CL  []string `json:"c"`
						V   []string `json:"v"`
						P   []string `json:"p"`
					} `json:"result"`
				}
				if err := getJSON(ctx, c, "https://api.kraken.com/0/public/Ticker?pair="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if len(r.Error) > 0 || len(r.Result) == 0 {
					return rawTicker{}, errNotListed
				}
				for _, d := range r.Result {
					var bid, ask, last float64
					if len(d.Bid) > 0 {
						bid = f(d.Bid[0])
					}
					if len(d.Ask) > 0 {
						ask = f(d.Ask[0])
					}
					if len(d.CL) > 0 {
						last = f(d.CL[0])
					}
					var volQuote float64
					if len(d.V) > 1 && len(d.P) > 1 {
						volQuote = f(d.V[1]) * f(d.P[1])
					}
					// Kraken's "o" is the UTC-day open, not a 24h base, so the
					// 24h change is deliberately left empty instead of invented.
					return rawTicker{Price: last, Bid: bid, Ask: ask, Change24h: nil, QuoteVolume: volQuote}, nil
				}
				return rawTicker{}, errNotListed
			},
		},
		{
			ID: "bitstamp", Name: "Bitstamp", Quote: "USDT",
			Market: func(b string) string { return strings.ToLower(b) + "usdt" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				body, err := getRaw(ctx, c, "https://www.bitstamp.net/api/v2/ticker/"+m+"/")
				if err != nil {
					return rawTicker{}, err
				}
				// Bitstamp answers an unsupported pair with a JSON array (e.g.
				// ["error", ...]) and HTTP 200, so decode the shape, not the status.
				if len(body) > 0 && body[0] == '[' {
					return rawTicker{}, errNotListed
				}
				var r struct {
					Last            string `json:"last"`
					Bid             string `json:"bid"`
					Ask             string `json:"ask"`
					PercentChange24 string `json:"percent_change_24"`
					Volume          string `json:"volume"`
					VWAP            string `json:"vwap"`
				}
				if err := json.Unmarshal(body, &r); err != nil {
					return rawTicker{}, err
				}
				if r.Last == "" {
					return rawTicker{}, errNotListed
				}
				return rawTicker{Price: f(r.Last), Bid: f(r.Bid), Ask: f(r.Ask), Change24h: pct(f(r.PercentChange24)), QuoteVolume: f(r.Volume) * f(r.VWAP)}, nil
			},
		},
		{
			ID: "cryptocom", Name: "Crypto.com", Quote: "USDT",
			Market: func(b string) string { return b + "_USDT" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var r struct {
					Code   int `json:"code"`
					Result struct {
						Data []struct {
							I  string `json:"i"`
							A  string `json:"a"`
							B  string `json:"b"`
							K  string `json:"k"`
							C  string `json:"c"`
							VV string `json:"vv"`
						} `json:"data"`
					} `json:"result"`
				}
				if err := getJSON(ctx, c, "https://api.crypto.com/exchange/v1/public/get-tickers?instrument_name="+m, &r); err != nil {
					return rawTicker{}, err
				}
				if r.Code != 0 || len(r.Result.Data) == 0 || r.Result.Data[0].A == "" {
					return rawTicker{}, errNotListed
				}
				d := r.Result.Data[0]
				return rawTicker{Price: f(d.A), Bid: f(d.B), Ask: f(d.K), Change24h: pct(f(d.C) * 100), QuoteVolume: f(d.VV)}, nil
			},
		},
		{
			ID: "bitfinex", Name: "Bitfinex", Quote: "USDT",
			Market: func(b string) string { return "t" + b + "UST" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				body, err := getRaw(ctx, c, "https://api-pub.bitfinex.com/v2/ticker/"+m)
				if err != nil {
					return rawTicker{}, err
				}
				// Flat array: [bid, bidSize, ask, askSize, dailyChange,
				// dailyChangeRelative, last, volume, high, low, timestamp]
				var r []*float64
				if err := json.Unmarshal(body, &r); err != nil || len(r) < 8 {
					return rawTicker{}, errNotListed
				}
				v := func(i int) float64 {
					if i < len(r) && r[i] != nil {
						return *r[i]
					}
					return 0
				}
				last := v(6)
				if last == 0 {
					return rawTicker{}, errNotListed
				}
				return rawTicker{Price: last, Bid: v(0), Ask: v(2), Change24h: pct(v(5) * 100), QuoteVolume: v(7) * last}, nil
			},
		},
		{
			ID: "coinbase", Name: "Coinbase", Quote: "USD",
			Market: func(b string) string { return b + "-USD" },
			Fetch: func(ctx context.Context, c *http.Client, m string) (rawTicker, error) {
				var t struct {
					Price  string `json:"price"`
					Bid    string `json:"bid"`
					Ask    string `json:"ask"`
					Volume string `json:"volume"`
				}
				if err := getJSON(ctx, c, "https://api.exchange.coinbase.com/products/"+m+"/ticker", &t); err != nil {
					return rawTicker{}, err
				}
				if t.Price == "" {
					return rawTicker{}, errNotListed
				}
				var s struct {
					Open   string `json:"open"`
					Volume string `json:"volume"`
					Last   string `json:"last"`
				}
				var ch *float64
				volQuote := f(t.Volume) * f(t.Price)
				if err := getJSON(ctx, c, "https://api.exchange.coinbase.com/products/"+m+"/stats", &s); err == nil {
					if open := f(s.Open); open > 0 {
						ch = pct((f(s.Last)/open - 1) * 100)
					}
					if s.Volume != "" {
						volQuote = f(s.Volume) * f(s.Last)
					}
				}
				return rawTicker{Price: f(t.Price), Bid: f(t.Bid), Ask: f(t.Ask), Change24h: ch, QuoteVolume: volQuote}, nil
			},
		},
	}
}
