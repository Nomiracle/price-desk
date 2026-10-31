package main

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// collector runs the same comparison the page does, on a timer, for every
// symbol in the catalog. The page remains a read-through cache; the collector
// is what turns "a table you can look at" into "a series that can be measured".
type collector struct {
	interval  time.Duration
	warmEvery time.Duration
	det       *detector

	mu        sync.Mutex
	cycles    int64
	lastAt    int64
	lastDur   int64
	lastOK    int
	startedAt time.Time
}

func newCollector(det *detector, interval, warm time.Duration) *collector {
	return &collector{interval: interval, warmEvery: warm, det: det, startedAt: time.Now()}
}

func (c *collector) Run(ctx context.Context) {
	for {
		start := time.Now()
		c.cycle(ctx)
		dur := time.Since(start)

		iv := c.interval
		// A symbol that can never form a consensus (too few centralised books)
		// must not pin the collector in fast-sampling mode forever.
		if c.det.Warming() && time.Since(c.startedAt) < 20*time.Minute {
			iv = c.warmEvery
		}
		wait := iv - dur
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *collector) cycle(ctx context.Context) {
	start := time.Now()
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var done int32

	for _, sym := range symbolCatalog {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("collector: %s panicked: %v", base, r)
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Cancellation is only advisory here: the per-venue HTTP calls use
			// their own short deadline, so a slow venue cannot pin the cycle.
			if ctx.Err() != nil {
				return
			}
			cmp := buildComparisonValue(base)
			c.det.Observe(base, cmp)
			atomic.AddInt32(&done, 1)
		}(sym.Base)
	}
	wg.Wait()

	c.pegs(ctx)

	now := time.Now()
	c.mu.Lock()
	c.cycles++
	c.lastAt = now.UnixMilli()
	c.lastDur = time.Since(start).Milliseconds()
	c.lastOK = int(done)
	cycles := c.cycles
	c.mu.Unlock()

	if cycles%5 == 0 {
		c.det.SavePriors()
	}
	if cycles%5 == 1 {
		log.Printf("collector: cycle #%d symbols=%d/%d dur=%dms", cycles, c.lastOK, len(symbolCatalog), c.lastDur)
	}
}

// pegs turns the USDT/USD and USDC/USD prints into an absolute peg monitor.
// These two are the assets whose depeg historically accompanies an exchange
// incident, and the page only ever used the USDT rate as a conversion factor.
func (c *collector) pegs(ctx context.Context) {
	pairs := []struct {
		sym, pair string
	}{
		{"USDT", "USDTZUSD"},
		{"USDC", "USDCUSD"},
	}
	for _, p := range pairs {
		var r struct {
			Error  []string `json:"error"`
			Result map[string]struct {
				CL []string `json:"c"`
			} `json:"result"`
		}
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		err := getJSON(cctx, httpClient, "https://api.kraken.com/0/public/Ticker?pair="+p.pair, &r)
		cancel()
		if err != nil || len(r.Error) > 0 {
			continue
		}
		for _, v := range r.Result {
			if len(v.CL) > 0 {
				if px := f(v.CL[0]); px > 0 {
					c.det.ObservePeg(p.sym, px)
				}
				break
			}
		}
	}
}

func (c *collector) Status() monitorStatus {
	st := c.det.Status(int(c.interval / time.Second))
	c.mu.Lock()
	st.Cycles = c.cycles
	st.LastCycleAt = c.lastAt
	st.LastCycleMS = c.lastDur
	if time.Since(c.startedAt) >= 20*time.Minute {
		st.Warmup = false
	}
	c.mu.Unlock()
	return st
}
