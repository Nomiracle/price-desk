package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type stubTransport func(*http.Request) (string, bool)

func (s stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, ok := s(req)
	if !ok {
		return nil, errors.New("unexpected request: " + req.URL.String())
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

const (
	fxBinance  = `{"symbol":"BTCUSDT","priceChangePercent":"-4.812","lastPrice":"77339.54000000","bidPrice":"77339.54000000","askPrice":"77339.55000000","quoteVolume":"2642228562.39894200"}`
	fxOKX      = `{"code":"0","data":[{"instId":"BTC-USDT","last":"77293.8","askPx":"77293.9","bidPx":"77293.8","open24h":"81246.1","volCcy24h":"1508445662.7"}]}`
	fxBybit    = `{"retCode":0,"result":{"list":[{"symbol":"BTCUSDT","bid1Price":"77294.1","ask1Price":"77294.2","lastPrice":"77294.1","price24hPcnt":"-0.0490","turnover24h":"1197279615.98"}]}}`
	fxBitget   = `{"code":"00000","data":[{"lastPr":"77339.51","bidPr":"77328.14","askPr":"77328.15","change24h":"-0.04806","quoteVolume":"596261541.67"}]}`
	fxGate     = `[{"currency_pair":"BTC_USDT","last":"77311.6","lowest_ask":"77311.6","highest_bid":"77311.5","change_percentage":"-4.84","quote_volume":"1658440887.11"}]`
	fxKuCoin   = `{"code":"200000","data":{"symbol":"BTC-USDT","buy":"77295","sell":"77295.1","changeRate":"-0.049","last":"77295","volValue":"619549330.64"}}`
	fxMEXC     = `{"symbol":"BTCUSDT","priceChangePercent":"-0.0491","lastPrice":"77265.57","bidPrice":"77318.47","askPrice":"77318.48","quoteVolume":"699845750.91"}`
	fxHTX      = `{"ch":"market.btcusdt.detail.merged","status":"ok","tick":{"open":81205.63,"close":77339.03,"low":76577.0,"high":81222.27,"amount":5999.04,"vol":468927151.347,"bid":[77345.28,0.018],"ask":[77349.65,0.018]}}`
	fxKraken   = `{"error":[],"result":{"XBTUSDT":{"a":["77287.90000","1","1.000"],"b":["77281.90000","1","1.000"],"c":["77306.60000","0.00038556"],"v":["211.27037246","286.94560825"],"p":["77521.05610","77935.22740"]}}}`
	fxBitstamp = `{"timestamp":"1793379352","open":"78216.81","last":"77214.73","volume":"12.15970459","vwap":"77870.41","bid":"77193.69","ask":"77277.65","percent_change_24":"-4.90","market_type":"SPOT"}`
	fxBitfinex = `[77310,6.75602083,77342,2.4467831,-3953,-0.04863794,77321,2212.58935251,81274,75699,1552298737974]`
	fxCrypto   = `{"code":0,"result":{"data":[{"i":"BTC_USDT","a":"77200.94","b":"77200.93","k":"77200.94","c":"-0.0501","vv":"248748699.84"}]}}`
	fxCBtick   = `{"ask":"77185.14","bid":"77183.59","volume":"16443.92428856","price":"77185.15","time":"2026-10-30T16:55:16Z"}`
	fxCStats   = `{"open":"81000","high":"81200","low":"76500","volume":"16443.92428856","last":"77185.15"}`
)

func testClient() *http.Client {
	return &http.Client{Transport: stubTransport(func(req *http.Request) (string, bool) {
		switch req.URL.Host {
		case "api.binance.com":
			return fxBinance, true
		case "www.okx.com":
			return fxOKX, true
		case "api.bybit.com":
			return fxBybit, true
		case "api.bitget.com":
			return fxBitget, true
		case "api.gateio.ws":
			return fxGate, true
		case "api.kucoin.com":
			return fxKuCoin, true
		case "api.mexc.com":
			return fxMEXC, true
		case "api.huobi.pro":
			return fxHTX, true
		case "api.kraken.com":
			return fxKraken, true
		case "www.bitstamp.net":
			return fxBitstamp, true
		case "api-pub.bitfinex.com":
			return fxBitfinex, true
		case "api.crypto.com":
			return fxCrypto, true
		case "api.exchange.coinbase.com":
			if strings.HasSuffix(req.URL.Path, "/stats") {
				return fxCStats, true
			}
			return fxCBtick, true
		}
		return "", false
	})}
}

func TestAdaptersParseBTCTickers(t *testing.T) {
	want := map[string]struct {
		price     float64
		change    float64
		hasChange bool
	}{
		"binance":   {77339.54, -4.812, true},
		"okx":       {77293.8, -4.866, true},
		"bybit":     {77294.1, -4.9, true},
		"bitget":    {77339.51, -4.806, true},
		"gate":      {77311.6, -4.84, true},
		"kucoin":    {77295, -4.9, true},
		"mexc":      {77265.57, -4.91, true},
		"htx":       {77339.03, -4.762, true},
		"kraken":    {77306.6, 0, false},
		"bitstamp":  {77214.73, -4.90, true},
		"bitfinex":  {77321, -4.863794, true},
		"cryptocom": {77200.94, -5.01, true},
		"coinbase":  {77185.15, -4.7097, true},
	}
	for _, a := range cexAdapters() {
		a := a
		t.Run(a.ID, func(t *testing.T) {
			raw, err := a.Fetch(context.Background(), testClient(), a.Market("BTC"))
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			w, ok := want[a.ID]
			if !ok {
				t.Fatalf("no expectation for %s", a.ID)
			}
			if raw.Price < w.price*0.999 || raw.Price > w.price*1.001 {
				t.Errorf("price = %v, want ~%v", raw.Price, w.price)
			}
			if raw.Bid <= 0 || raw.Ask <= 0 {
				t.Errorf("bid/ask missing: %v/%v", raw.Bid, raw.Ask)
			}
			if w.hasChange {
				if raw.Change24h == nil {
					t.Fatalf("change missing")
				}
				if *raw.Change24h < w.change-0.02 || *raw.Change24h > w.change+0.02 {
					t.Errorf("change = %v, want ~%v", *raw.Change24h, w.change)
				}
			} else if raw.Change24h != nil {
				t.Errorf("change should be nil, got %v", *raw.Change24h)
			}
			if raw.QuoteVolume <= 0 {
				t.Errorf("quote volume missing")
			}
		})
	}
}

func TestBitstampArrayMeansNotListed(t *testing.T) {
	c := &http.Client{Transport: stubTransport(func(*http.Request) (string, bool) {
		return `[{"pair":"BTC/USD","last":"77214.73"}]`, true
	})}
	var bs *adapter
	for i, a := range cexAdapters() {
		if a.ID == "bitstamp" {
			bs = &cexAdapters()[i]
		}
	}
	if bs == nil {
		t.Fatal("bitstamp adapter missing")
	}
	_, err := bs.Fetch(context.Background(), c, "solusdt")
	if !errors.Is(err, errNotListed) {
		t.Fatalf("err = %v, want errNotListed", err)
	}
}

func TestSummaryCrossSpreadAndMedian(t *testing.T) {
	quotes := []ticker{
		{Exchange: "a", Name: "A", PriceUSDT: 100, BidUSDT: 99.90, AskUSDT: 99.95},
		{Exchange: "b", Name: "B", PriceUSDT: 100.2, BidUSDT: 100.30, AskUSDT: 100.35},
		{Exchange: "c", Name: "C", PriceUSDT: 100.1, BidUSDT: 100.05, AskUSDT: 100.10},
		{Exchange: "d", Name: "D"},
	}
	quotes[3].Error = "timeout"
	s := summarise(quotes)
	if s.OkCount != 3 || s.TotalCount != 4 {
		t.Fatalf("counts = %d/%d", s.OkCount, s.TotalCount)
	}
	if s.MedianPrice != 100.1 {
		t.Errorf("median = %v, want 100.1", s.MedianPrice)
	}
	if s.BestAsk.Exchange != "a" || s.BestBid.Exchange != "b" {
		t.Errorf("best legs = %v/%v", s.BestAsk.Exchange, s.BestBid.Exchange)
	}
	// (100.30 - 99.95) / ((100.30+99.95)/2) * 10000 = 34.956...
	if s.CrossSpreadBps < 34.9 || s.CrossSpreadBps > 35.0 {
		t.Errorf("cross spread = %v bps", s.CrossSpreadBps)
	}
	// (100.2/100 - 1) * 10000 = 20 bps
	if s.DispersionBps < 19.9 || s.DispersionBps > 20.1 {
		t.Errorf("dispersion = %v bps", s.DispersionBps)
	}
}

func TestSummaryNoData(t *testing.T) {
	s := summarise([]ticker{{Exchange: "a", NotListed: true}, {Exchange: "b", Error: "boom"}})
	if s.OkCount != 0 || s.MedianPrice != 0 || s.BestAsk != nil || s.BestBid != nil {
		t.Fatalf("unexpected summary for empty input: %+v", s)
	}
}
