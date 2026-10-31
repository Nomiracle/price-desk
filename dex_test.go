package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const (
	addrWBTCeth  = "0x2260fac5e5542a773aa44fbcfedf7c193bc2c599"
	addrWETHbase = "0x4200000000000000000000000000000000000006"
	addrSOL      = "So11111111111111111111111111111111111111112"
)

func resetDEXCache() {
	dexMu.Lock()
	dexCache = map[string]dexCacheEntry{}
	dexMu.Unlock()
}

func adapterByID(t *testing.T, id string) adapter {
	t.Helper()
	for _, a := range dexAdapters() {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("dex adapter %s missing", id)
	return adapter{}
}

// fixture pairs mimic the upstream shape: 30 pairs across chains and venues,
// including an impostor contract that must never be treated as the real token.
const dexFixture = `{"schemaVersion":"1.0.0","pairs":[
 {"chainId":"ethereum","dexId":"uniswap","pairAddress":"0xaaa","baseToken":{"address":"0x2260fac5e5542a773aa44fbcfedf7c193bc2c599","symbol":"WBTC"},"quoteToken":{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","symbol":"USDT"},"priceUsd":"77205.86","priceChange":{"h24":-4.65},"volume":{"h24":532100000},"liquidity":{"usd":24400000}},
 {"chainId":"ethereum","dexId":"sushiswap","pairAddress":"0xbbb","baseToken":{"address":"0x2260fac5e5542a773aa44fbcfedf7c193bc2c599","symbol":"WBTC"},"quoteToken":{"address":"0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2","symbol":"WETH"},"priceUsd":"76000","priceChange":{"h24":-4.1},"volume":{"h24":90000000},"liquidity":{"usd":90000000}},
 {"chainId":"ethereum","dexId":"uniswap","pairAddress":"0xccc","baseToken":{"address":"0xdeadbeef00000000000000000000000000000001","symbol":"WBTC"},"quoteToken":{"address":"0xdac17f958d2ee523a2206206994597c13d831ec7","symbol":"USDT"},"priceUsd":"1","priceChange":{"h24":0},"volume":{"h24":1000000},"liquidity":{"usd":200000000}},
 {"chainId":"base","dexId":"uniswap","pairAddress":"0xddd","baseToken":{"address":"0x4200000000000000000000000000000000000006","symbol":"WETH"},"quoteToken":{"address":"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913","symbol":"USDC"},"priceUsd":"2242.40","volume":{"h24":81200000},"liquidity":{"usd":150550000}},
 {"chainId":"base","dexId":"aerodrome","pairAddress":"0xeee","baseToken":{"address":"0x4200000000000000000000000000000000000006","symbol":"WETH"},"quoteToken":{"address":"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913","symbol":"USDC"},"priceUsd":"2243.25","priceChange":{"h24":-4.54},"volume":{"h24":4100000},"liquidity":{"usd":4160000}},
 {"chainId":"solana","dexId":"raydium","pairAddress":"sol-1","baseToken":{"address":"So11111111111111111111111111111111111111112","symbol":"SOL"},"quoteToken":{"address":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","symbol":"USDC"},"priceUsd":"95.14","priceChange":{"h24":-6.6},"volume":{"h24":390000000},"liquidity":{"usd":20170000}},
 {"chainId":"solana","dexId":"orca","pairAddress":"sol-2","baseToken":{"address":"So11111111111111111111111111111111111111112","symbol":"SOL"},"quoteToken":{"address":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","symbol":"USDC"},"priceUsd":"94.88","priceChange":{"h24":-6.7},"volume":{"h24":410000000},"liquidity":{"usd":25640000}}
]}`

func dexTestClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: stubTransport(func(req *http.Request) (string, bool) {
		if strings.HasPrefix(req.URL.String(), dexEndpoint) {
			return dexFixture, true
		}
		return "", false
	})}
}

func TestDEXPicksVenuesOwnDeepestPool(t *testing.T) {
	resetDEXCache()
	defer resetDEXCache()
	c := dexTestClient(t)

	cases := []struct {
		id     string
		base   string
		price  float64
		market string
		chain  string
		change float64
		hasChg bool
		minLiq float64
	}{
		{"uniswap-eth", "BTC", 77205.86, "WBTC/USDT", "ethereum", -4.65, true, 24_400_000},
		{"uniswap-base", "ETH", 2242.40, "WETH/USDC", "base", 0, false, 150_550_000},
		{"aerodrome-base", "ETH", 2243.25, "WETH/USDC", "base", -4.54, true, 4_160_000},
		{"raydium", "SOL", 95.14, "SOL/USDC", "solana", -6.6, true, 20_170_000},
		{"orca", "SOL", 94.88, "SOL/USDC", "solana", -6.7, true, 25_640_000},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			a := adapterByID(t, tc.id)
			if a.Kind != "dex" || a.Quote != "USD" {
				t.Fatalf("adapter kind/quote = %q/%q", a.Kind, a.Quote)
			}
			raw, err := a.Fetch(context.Background(), c, a.Market(tc.base))
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if raw.Price != tc.price {
				t.Errorf("price = %v, want %v", raw.Price, tc.price)
			}
			if raw.MarketLabel != tc.market {
				t.Errorf("market label = %q, want %q", raw.MarketLabel, tc.market)
			}
			if tc.hasChg {
				if raw.Change24h == nil || *raw.Change24h != tc.change {
					t.Errorf("change = %v, want %v", raw.Change24h, tc.change)
				}
			} else if raw.Change24h != nil {
				t.Errorf("change = %v, want nil", *raw.Change24h)
			}
			if raw.LiquidityUSD < tc.minLiq {
				t.Errorf("liquidity = %v, want >= %v", raw.LiquidityUSD, tc.minLiq)
			}
			if raw.Bid != 0 || raw.Ask != 0 {
				t.Errorf("pool venue must not invent a book: bid/ask %v/%v", raw.Bid, raw.Ask)
			}
		})
	}
}

func TestDEXPancakeSwapOnBSC(t *testing.T) {
	resetDEXCache()
	defer resetDEXCache()
	// A BTCB pool on BSC lives on a different address than WBTC on Ethereum.
	const bscFixture = `{"pairs":[{"chainId":"bsc","dexId":"pancakeswap","pairAddress":"0xbbb","baseToken":{"address":"0x7130d2a12b9bcbfae4f2634d864a1ee1ce3ead9c","symbol":"BTCB"},"quoteToken":{"address":"0xbb4cdb9cbd36b01bd1cbaebf2de08d9173bc095c","symbol":"WBNB"},"priceUsd":"77638.74","priceChange":{"h24":-4.25},"volume":{"h24":12000000},"liquidity":{"usd":8200000}}]}`
	c := &http.Client{Transport: stubTransport(func(*http.Request) (string, bool) { return bscFixture, true })}
	a := adapterByID(t, "pancakeswap-bsc")
	raw, err := a.Fetch(context.Background(), c, a.Market("BTC"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if raw.Price != 77638.74 || raw.MarketLabel != "BTCB/WBNB" {
		t.Fatalf("price/market = %v/%q", raw.Price, raw.MarketLabel)
	}
}

func TestDEXNotListedWithoutVerifiedContract(t *testing.T) {
	resetDEXCache()
	defer resetDEXCache()
	a := adapterByID(t, "uniswap-eth")
	addr := a.Market("XRP") // no verified ERC20 market for XRP in the registry
	if addr != "" {
		t.Fatalf("registry unexpectedly has XRP on ethereum: %s", addr)
	}
	c := &http.Client{Transport: stubTransport(func(req *http.Request) (string, bool) {
		t.Fatalf("unexpected upstream call for an unknown token: %s", req.URL)
		return "", false
	})}
	if _, err := a.Fetch(context.Background(), c, addr); !errors.Is(err, errNotListed) {
		t.Fatalf("err = %v, want errNotListed", err)
	}
}

func TestDEXNotListedWhenVenueHasNoPool(t *testing.T) {
	resetDEXCache()
	defer resetDEXCache()
	// Base ETH is priced on uniswap + aerodrome in the fixture, never on
	// PancakeSwap, so the BSC-scoped adapter must not borrow that price.
	a := newDEXAdapter("pancake-base", "PancakeSwap", "base", "pancakeswap")
	_, err := a.Fetch(context.Background(), dexTestClient(t), addrWETHbase)
	if !errors.Is(err, errNotListed) {
		t.Fatalf("err = %v, want errNotListed", err)
	}
}

func TestDEXCacheServesBothVenuesOnOneChain(t *testing.T) {
	resetDEXCache()
	defer resetDEXCache()
	var mu sync.Mutex
	calls := 0
	c := &http.Client{Transport: stubTransport(func(req *http.Request) (string, bool) {
		mu.Lock()
		calls++
		mu.Unlock()
		return dexFixture, true
	})}
	for _, id := range []string{"uniswap-base", "aerodrome-base"} {
		a := adapterByID(t, id)
		if _, err := a.Fetch(context.Background(), c, a.Market("ETH")); err != nil {
			t.Fatalf("%s fetch: %v", id, err)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (same token address, shared cache)", calls)
	}
}

func TestDEXRegistryShapes(t *testing.T) {
	evm := regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	base58 := regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
	for chain, toks := range dexTokens {
		if _, ok := dexChainNames[chain]; !ok {
			t.Errorf("chain %q has no display name", chain)
		}
		for sym, addr := range toks {
			if sym != strings.ToUpper(sym) {
				t.Errorf("%s/%s: symbol is not upper case", chain, sym)
			}
			if chain == "solana" {
				if !base58.MatchString(addr) {
					t.Errorf("%s/%s: %q is not a base58 mint", chain, sym, addr)
				}
			} else if !evm.MatchString(addr) {
				t.Errorf("%s/%s: %q is not an EVM address", chain, sym, addr)
			}
		}
	}
	// A few anchors that must resolve, so a typo in the registry fails loudly.
	for _, tc := range []struct{ chain, sym, want string }{
		{"ethereum", "BTC", addrWBTCeth},
		{"base", "ETH", addrWETHbase},
		{"solana", "SOL", addrSOL},
	} {
		got := dexTokenAddress(tc.chain, strings.ToLower(tc.sym))
		if !sameAddress(got, tc.want) {
			t.Errorf("%s/%s = %q, want %q", tc.chain, tc.sym, got, tc.want)
		}
	}
	if got := dexTokenAddress("ethereum", "XRP"); got != "" {
		t.Errorf("XRP on ethereum = %q, want empty", got)
	}
}

func TestDEXAdaptersHaveDistinctIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range dexAdapters() {
		if seen[a.ID] {
			t.Fatalf("duplicate dex adapter id %q", a.ID)
		}
		seen[a.ID] = true
		if a.Name == "" || a.Chain == "" || a.ChainName == "" {
			t.Errorf("%s: incomplete adapter metadata", a.ID)
		}
	}
	// IDs must also not collide with the centralised venues (history keys).
	for _, a := range cexAdapters() {
		if seen[a.ID] {
			t.Fatalf("dex id %q collides with a cex id", a.ID)
		}
	}
}

func mustPairs(t *testing.T, raw string) []dexPair {
	t.Helper()
	var body struct {
		Pairs []dexPair `json:"pairs"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("fixture json: %v", err)
	}
	return body.Pairs
}

func TestDEXPrefersTradedPoolOverStaleDeeperPool(t *testing.T) {
	const bscDOGE = "0xba2ae424d960c26247dd6c32edc70b295c744c43"
	// DOGE/COSA is the deepest pool but its 24h volume is dust (16 USD): its
	// price is a stale print. DOGE/WBNB traded, so it is the one to show.
	pairs := mustPairs(t, `{"pairs":[
	 {"chainId":"bsc","dexId":"pancakeswap","baseToken":{"address":"0xba2ae424d960c26247dd6c32edc70b295c744c43","symbol":"DOGE"},"quoteToken":{"symbol":"COSA"},"priceUsd":"0.07575","priceChange":{"h24":-4.1},"volume":{"h24":16},"liquidity":{"usd":1290000}},
	 {"chainId":"bsc","dexId":"pancakeswap","baseToken":{"address":"0xba2ae424d960c26247dd6c32edc70b295c744c43","symbol":"DOGE"},"quoteToken":{"symbol":"WBNB"},"priceUsd":"0.07571","priceChange":{"h24":-4.2},"volume":{"h24":150000},"liquidity":{"usd":860000}}
	]}`)
	p := bestDEXPair(pairs, "bsc", []string{"pancakeswap"}, bscDOGE)
	if p == nil {
		t.Fatal("no pair selected")
	}
	if p.QuoteToken.Symbol != "WBNB" || p.PriceUSD != "0.07571" {
		t.Fatalf("selected %s at %s, want WBNB at 0.07571", p.QuoteToken.Symbol, p.PriceUSD)
	}
	// When nothing traded, the deepest pool is still better than no price.
	all := mustPairs(t, `{"pairs":[
	 {"chainId":"bsc","dexId":"pancakeswap","baseToken":{"address":"0xba2ae424d960c26247dd6c32edc70b295c744c43"},"quoteToken":{"symbol":"COSA"},"priceUsd":"0.07575","volume":{"h24":16},"liquidity":{"usd":1290000}},
	 {"chainId":"bsc","dexId":"pancakeswap","baseToken":{"address":"0xba2ae424d960c26247dd6c32edc70b295c744c43"},"quoteToken":{"symbol":"WBNB"},"priceUsd":"0.07571","volume":{"h24":0},"liquidity":{"usd":860000}}
	]}`)
	if got := bestDEXPair(all, "bsc", []string{"pancakeswap"}, bscDOGE); got == nil || got.QuoteToken.Symbol != "COSA" {
		t.Fatalf("fallback selection = %+v, want the deepest pool", got)
	}
}

func TestThinPoolThreshold(t *testing.T) {
	cases := []struct {
		kind string
		liq  float64
		want bool
	}{
		{"dex", 0, false}, // unknown liquidity is not a thin verdict
		{"dex", dexThinLiquidityUSD - 1, true},
		{"dex", dexThinLiquidityUSD, false},
		{"cex", 0, false}, // books have no liquidity figure
	}
	for _, c := range cases {
		if got := thinPool(c.kind, c.liq); got != c.want {
			t.Errorf("thinPool(%q, %v) = %v, want %v", c.kind, c.liq, got, c.want)
		}
	}
}

func TestSummarySkipsThinPools(t *testing.T) {
	quotes := []ticker{
		{Exchange: "uniswap-eth", Kind: "dex", PriceUSDT: 100, LiquidityUSD: 40_000_000},
		{Exchange: "raydium", Kind: "dex", PriceUSDT: 60, LiquidityUSD: 160_000, LowLiquidity: true},
		{Exchange: "binance", Kind: "cex", PriceUSDT: 101, BidUSDT: 100.9, AskUSDT: 101.1},
	}
	s := summarise(quotes)
	if s.OkCount != 2 {
		t.Fatalf("ok count = %d, want 2 (thin pool excluded)", s.OkCount)
	}
	if s.MedianPrice != 100.5 {
		t.Fatalf("median = %v, want 100.5", s.MedianPrice)
	}
	if s.BestBid == nil || s.BestBid.Exchange != "binance" {
		t.Fatalf("best bid = %+v, want binance (DEX has no book)", s.BestBid)
	}
}
