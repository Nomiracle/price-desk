package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Decentralised venues are priced from on-chain pools indexed by DexScreener
// (keyless public API). A pool has no order book, so bid/ask stay empty and the
// price is the pool's latest traded price ("priceUsd").
//
// A symbol only appears on a chain when its canonical contract is listed in
// dexTokens. Symbol search is deliberately NOT used: a search for "BTC" returns
// dozens of impostor tokens, and the most liquid looking one is often a
// self-reported pool. Addresses below were verified against the live API.

const dexEndpoint = "https://api.dexscreener.com/latest/dex/tokens/"

var dexChainNames = map[string]string{
	"ethereum": "Ethereum",
	"bsc":      "BNB Chain",
	"base":     "Base",
	"arbitrum": "Arbitrum",
	"solana":   "Solana",
}

var dexTokens = map[string]map[string]string{
	"ethereum": {
		"BTC":  "0x2260fac5e5542a773aa44fbcfedf7c193bc2c599", // WBTC
		"ETH":  "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", // WETH
		"BNB":  "0xB8c77482e45F1F44dE1745F52C74426C631bDD52", // BNB (ERC20)
		"LINK": "0x514910771AF9Ca656af840dff83E8264EcF986CA",
		"UNI":  "0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984",
		"AAVE": "0x7Fc66500c84A76Ad7e9c93437bFc5Ac33E2DDaE9",
		"PEPE": "0x6982508145454Ce325dDbE47a25d4ec3d2311933",
		"ARB":  "0xB50721BCf8d664c30412Cfbc6cf7a15145234ad1",
		"WLD":  "0x163f8C2467924be0ae7B5347228CABF260318753",
		"SOL":  "0xD31a59c85aE9D8edEFeC411D448f90841571b89c", // Wormhole SOL
		"NEAR": "0x85F17Cf997934a597031b2E18a9aB6ebD4B9f6a4", // Wormhole NEAR
	},
	"bsc": {
		"BTC":  "0x7130d2A12B9BCbFAe4f2634d864A1Ee1Ce3Ead9c", // BTCB
		"ETH":  "0x2170Ed0880ac9A755fd29B2688956BD959F933F8",
		"BNB":  "0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c", // WBNB
		"LINK": "0xF8A0BF9cF54Bb92F17374d9e9A321E6a111a51bD",
		"UNI":  "0xBf5140A22578168FD562DCcF235E5D43A02ce9B1",
		"DOGE": "0xbA2aE424d960c26247Dd6c32edC70B295c744C43",
		"XRP":  "0x1D2F0da169ceB9fC7B3144628dB156f3F6c60dBE",
		"ADA":  "0x3EE2200Efb3400fAbB9AacF31297cBdD1d435D47",
		"DOT":  "0x7083609fCE4d1d8Dc0C979AAb8c869Ea2C873402",
		"AVAX": "0x1CE0c2827e2eF14D5C4f29a091d735A204794041",
		"LTC":  "0x4338665CBB7B2485A8855A139b75D5e34AB0DB94",
		"BCH":  "0x8fF795a6F4D97E7887C79beA79aba5cc76444aDf",
	},
	"base": {
		"ETH":  "0x4200000000000000000000000000000000000006", // WETH predeploy
		"BTC":  "0xcbB7C0000aB88B473b1f5aFd9ef808440eed33Bf", // cbBTC
		"LINK": "0x88Fb150BDc53A65fe94Dea0c9BA0a6dAf8C6e196",
		"AERO": "0x940181a94A35A4569E4529A3CDfB74e38FD98631",
	},
	"arbitrum": {
		"BTC":  "0x2f2a2543B76A4166549F7aaB2e75Bef0aefC5B0f", // WBTC
		"ETH":  "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1", // WETH
		"ARB":  "0x912CE59144191C1204E64559FE8253a0e49E6548",
		"LINK": "0xf97f4df75117a78c1A5a0DBb814Af92458539FB4",
		"UNI":  "0xfa7F8980b0f1E64A2062791cc3b0871572f1F7f0",
	},
	"solana": {
		"SOL": "So11111111111111111111111111111111111111112",  // wrapped SOL
		"ETH": "7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", // Wormhole WETH
		"BTC": "3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", // Wormhole WBTC
	},
}

// dexTokenAddress returns the canonical contract for base on chain, or "" when
// this chain has no verified contract for the symbol.
func dexTokenAddress(chain, base string) string {
	return dexTokens[chain][strings.ToUpper(base)]
}

type dexPair struct {
	ChainID     string `json:"chainId"`
	DexID       string `json:"dexId"`
	PairAddress string `json:"pairAddress"`
	PriceUSD    string `json:"priceUsd"`
	PriceChange struct {
		H24 *float64 `json:"h24"`
	} `json:"priceChange"`
	Volume struct {
		H24 float64 `json:"h24"`
	} `json:"volume"`
	Liquidity struct {
		USD float64 `json:"usd"`
	} `json:"liquidity"`
	BaseToken struct {
		Address string `json:"address"`
		Symbol  string `json:"symbol"`
	} `json:"baseToken"`
	QuoteToken struct {
		Address string `json:"address"`
		Symbol  string `json:"symbol"`
	} `json:"quoteToken"`
}

// dexCache keeps the 30-pair response per token address for a few seconds.
// Every venue on the same chain shares one token address, so a comparison
// makes one upstream call per chain instead of one per venue.
var (
	dexMu    sync.Mutex
	dexCache = map[string]dexCacheEntry{}
)

const dexCacheTTL = 4 * time.Second

type dexCacheEntry struct {
	at    time.Time
	pairs []dexPair
}

func dexPairs(ctx context.Context, c *http.Client, addr string) ([]dexPair, error) {
	dexMu.Lock()
	if e, ok := dexCache[addr]; ok && time.Since(e.at) < dexCacheTTL {
		dexMu.Unlock()
		return e.pairs, nil
	}
	dexMu.Unlock()

	var body struct {
		Pairs []dexPair `json:"pairs"`
	}
	if err := getJSON(ctx, c, dexEndpoint+addr, &body); err != nil {
		return nil, err
	}
	if body.Pairs == nil {
		body.Pairs = []dexPair{}
	}
	dexMu.Lock()
	dexCache[addr] = dexCacheEntry{at: time.Now(), pairs: body.Pairs}
	dexMu.Unlock()
	return body.Pairs, nil
}

// dexActiveVolumeUSD marks a pool as actually trading. A pool with a single
// dust swap in 24h reports non-zero volume but its "price" is still a stale
// print, so volume is compared against this floor rather than against zero.
const dexActiveVolumeUSD = 1000

// dexStableQuotes are quote assets whose USD price is ~1, so a pool against
// one of them is a direct USD reference for the base token. A pool quoted in a
// volatile asset (SOL/WETH/JLP/JitoSOL) derives its USD price through that
// asset; an upstream stale reserve snapshot of such a pool then looks exactly
// like a venue dislocation.
var dexStableQuotes = map[string]bool{
	"USD": true, "USDC": true, "USDT": true, "DAI": true, "FDUSD": true,
	"USDS": true, "PYUSD": true, "USDE": true,
}

// dexCrossCheckBps is how far a volatile-quoted pool may sit from the same
// token's USD-stable pool on the same venue before the print is treated as a
// data defect rather than a venue price. A real venue dislocation moves every
// pool on that venue together, so this suppresses stale snapshots without
// hiding an actual event.
const dexCrossCheckBps = 100.0

// dexRefMinLiquidityUSD keeps a dust pool from becoming the reference. The
// reference only has to be a usable USD print, not the deepest pool.
const dexRefMinLiquidityUSD = 25_000

// stableDEXPair returns the deepest pool on this venue quoted in a USD-stable
// asset. It is the independent USD check for a volatile-quoted pool.
func stableDEXPair(pairs []dexPair, chain string, dexIDs []string, addr string) *dexPair {
	var out []dexPair
	for _, p := range pairs {
		if p.ChainID != chain || !containsString(dexIDs, p.DexID) {
			continue
		}
		if !sameAddress(p.BaseToken.Address, addr) {
			continue
		}
		if !dexStableQuotes[strings.ToUpper(p.QuoteToken.Symbol)] {
			continue
		}
		if p.Volume.H24 < dexActiveVolumeUSD || p.Liquidity.USD < dexRefMinLiquidityUSD {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Liquidity.USD > out[j].Liquidity.USD })
	return &out[0]
}

// shortPool keeps an alert readable; the full address stays available on the
// comparison page.
func shortPool(addr string) string {
	if len(addr) > 10 {
		return addr[:10] + "…"
	}
	return addr
}

// bestDEXPair picks the deepest pool that the venue actually runs on this
// chain. Anything else (another chain, another venue, an impostor contract) is
// ignored rather than shown as this venue's price. Pools that traded are
// preferred; a deeper pool with dust volume is often an abandoned or
// manipulated one.
func bestDEXPair(pairs []dexPair, chain string, dexIDs []string, addr string) *dexPair {
	var out, active []dexPair
	for _, p := range pairs {
		if p.ChainID != chain {
			continue
		}
		if !containsString(dexIDs, p.DexID) {
			continue
		}
		if !sameAddress(p.BaseToken.Address, addr) {
			continue
		}
		out = append(out, p)
		if p.Volume.H24 >= dexActiveVolumeUSD {
			active = append(active, p)
		}
	}
	if len(active) > 0 {
		out = active
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Liquidity.USD > out[j].Liquidity.USD })
	return &out[0]
}

// sameAddress treats EVM addresses case-insensitively and base58 (Solana)
// addresses exactly, because base58 is case-sensitive.
func sameAddress(a, b string) bool {
	if strings.HasPrefix(a, "0x") || strings.HasPrefix(b, "0x") {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func newDEXAdapter(id, name, chain string, dexIDs ...string) adapter {
	return adapter{
		ID:        id,
		Name:      name,
		Kind:      "dex",
		Chain:     chain,
		ChainName: dexChainNames[chain],
		Quote:     "USD",
		Market: func(base string) string {
			return dexTokenAddress(chain, base)
		},
		Fetch: func(ctx context.Context, c *http.Client, addr string) (rawTicker, error) {
			if addr == "" {
				return rawTicker{}, errNotListed
			}
			pairs, err := dexPairs(ctx, c, addr)
			if err != nil {
				return rawTicker{}, err
			}
			p := bestDEXPair(pairs, chain, dexIDs, addr)
			if p == nil {
				return rawTicker{}, errNotListed
			}
			price := f(p.PriceUSD)
			if price <= 0 {
				return rawTicker{}, errNotListed
			}
			t := rawTicker{
				Price:        price,
				Change24h:    p.PriceChange.H24,
				QuoteVolume:  p.Volume.H24,
				LiquidityUSD: p.Liquidity.USD,
				MarketLabel:  p.BaseToken.Symbol + "/" + p.QuoteToken.Symbol,
				Pool:         p.PairAddress,
			}
			// A pool quoted in a volatile asset gets its USD price by chaining
			// through that asset, so an upstream stale reserve snapshot shows
			// up as a large, single-venue "depeg". Cross-check the print
			// against the same token's USD-stable pool on the same venue: a
			// real dislocation moves both pools together, a stale snapshot
			// does not.
			if !dexStableQuotes[strings.ToUpper(p.QuoteToken.Symbol)] {
				if ref := stableDEXPair(pairs, chain, dexIDs, addr); ref != nil {
					if refPrice := f(ref.PriceUSD); refPrice > 0 {
						diff := (price/refPrice - 1) * 10000
						if math.Abs(diff) > dexCrossCheckBps {
							t.Suspect = true
							t.SuspectNote = fmt.Sprintf("%s %s 报 %.6g，相差 %+.0f bps",
								ref.BaseToken.Symbol+"/"+ref.QuoteToken.Symbol, shortPool(ref.PairAddress), refPrice, diff)
						}
					}
				}
			}
			return t, nil
		},
	}
}

// dexAdapters lists the on-chain venues shown next to the centralised books.
// Uniswap is repeated per chain because the same protocol prices different
// pools on each network; PancakeSwap (BSC), Aerodrome (Base) and the two
// Solana AMMs cover the rest.
func dexAdapters() []adapter {
	return []adapter{
		newDEXAdapter("uniswap-eth", "Uniswap", "ethereum", "uniswap"),
		newDEXAdapter("uniswap-base", "Uniswap", "base", "uniswap"),
		newDEXAdapter("uniswap-arb", "Uniswap", "arbitrum", "uniswap"),
		newDEXAdapter("pancakeswap-bsc", "PancakeSwap", "bsc", "pancakeswap"),
		newDEXAdapter("aerodrome-base", "Aerodrome", "base", "aerodrome", "aero"),
		newDEXAdapter("raydium", "Raydium", "solana", "raydium"),
		newDEXAdapter("orca", "Orca", "solana", "orca"),
	}
}
