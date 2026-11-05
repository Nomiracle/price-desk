package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// The detector's job is to stay quiet until a venue's deviation is both
// unusual for that venue and large in absolute terms. These tests pin the two
// ways that can go wrong: firing during warm-up / on noise, and swallowing a
// second, larger dislocation because of the cooldown.

var testVenues = []struct {
	id, name, kind string
}{
	{"binance", "Binance", "cex"},
	{"okx", "OKX", "cex"},
	{"bybit", "Bybit", "cex"},
	{"kraken", "Kraken", "cex"},
	{"coinbase", "Coinbase", "cex"},
	{"gate", "Gate", "cex"},
	{"uniswap-eth", "Uniswap", "dex"},
}

// snapshot builds a comparison where every venue sits at base*(1+bps/1e4).
// Venues named in broken carry an error instead of a price.
func snapshot(base string, bps map[string]float64, broken map[string]string) comparison {
	c := comparison{Base: base, Quote: "USDT", FetchedAt: time.Now().UnixMilli()}
	for _, v := range testVenues {
		q := ticker{Exchange: v.id, Name: v.name, Kind: v.kind, Quote: "USDT"}
		if msg, bad := broken[v.id]; bad {
			q.Error = msg
		} else {
			q.Price = 700 * (1 + bps[v.id]/10000)
			q.PriceUSDT = q.Price
			q.BidUSDT = q.Price * 0.9999
			q.AskUSDT = q.Price * 1.0001
		}
		c.Quotes = append(c.Quotes, q)
	}
	c.Summary = summarise(c.Quotes)
	return c
}

func feed(t *testing.T, d *detector, n int, bps map[string]float64) []Event {
	t.Helper()
	var out []Event
	for i := 0; i < n; i++ {
		out = append(out, d.Observe("BTC", snapshot("BTC", bps, nil))...)
	}
	return out
}

func TestMedianMADIgnoresSingleOutlier(t *testing.T) {
	med, mad := medianMAD([]float64{0, 0, 0, 0, 0, 0, 300})
	if med != 0 {
		t.Fatalf("median = %v, want 0", med)
	}
	if mad > 1 {
		t.Fatalf("mad = %v, want ~0; one print must not inflate the baseline", mad)
	}
}

func TestDetectorSilentDuringWarmup(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	ev := feed(t, d, 10, map[string]float64{})
	if len(ev) != 0 {
		t.Fatalf("warm-up produced %d events, want 0: %+v", len(ev), ev[0])
	}
	if !d.Warming() {
		t.Fatal("detector should still report warming")
	}
}

func TestDetectorFiresAfterConfirmAndRespectsCooldown(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	feed(t, d, 16, map[string]float64{})

	// One print is not an event.
	if ev := feed(t, d, 1, map[string]float64{"binance": 120}); len(ev) != 0 {
		t.Fatalf("single anomalous sample fired %d events", len(ev))
	}
	ev := feed(t, d, 1, map[string]float64{"binance": 120})
	if len(ev) != 1 || ev[0].Kind != kindVenueDepeg || ev[0].Venue != "binance" {
		t.Fatalf("second anomalous sample: %+v", ev)
	}
	if ev[0].Severity != "alert" {
		t.Fatalf("severity = %q, want alert (2x threshold)", ev[0].Severity)
	}
	if math.Abs(ev[0].DevBps-120) > 1 {
		t.Fatalf("dev = %.1f, want ~120", ev[0].DevBps)
	}

	// Same dislocation again: inside the cooldown, no duplicate.
	if ev := feed(t, d, 2, map[string]float64{"binance": 120}); len(ev) != 0 {
		t.Fatalf("cooldown did not suppress duplicate: %+v", ev[0])
	}

	// A materially larger move is new information and must get through.
	ev = feed(t, d, 2, map[string]float64{"binance": 300})
	if len(ev) != 1 || ev[0].Kind != kindVenueDepeg {
		t.Fatalf("larger move suppressed: %+v", ev)
	}

	// Returning inside the band clears state so the next dislocation is not
	// swallowed by the old cooldown.
	ev = feed(t, d, 1, map[string]float64{})
	if len(ev) != 1 || ev[0].Kind != kindVenueNormal {
		t.Fatalf("expected venue_normal, got %+v", ev)
	}
}

func TestDetectorIgnoresPersistentPremium(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	// A venue that always trades 60bps rich is not an event: its own baseline
	// absorbs the level, only the change matters.
	ev := feed(t, d, 30, map[string]float64{"kraken": 60})
	for _, e := range ev {
		if e.Kind == kindVenueDepeg && e.Venue == "kraken" {
			t.Fatalf("persistent premium flagged: %+v", e)
		}
	}
}

func TestDetectorDispersionEvent(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	for i := 0; i < 20; i++ {
		c := snapshot("BTC", map[string]float64{"binance": 40, "okx": -40}, nil)
		d.Observe("BTC", c)
	}
	var ev []Event
	for i := 0; i < 2; i++ {
		c := snapshot("BTC", map[string]float64{"binance": 400, "okx": -400}, nil)
		ev = append(ev, d.Observe("BTC", c)...)
	}
	found := false
	for _, e := range ev {
		if e.Kind == kindDispersion {
			found = true
		}
	}
	if !found {
		t.Fatalf("wide dispersion not reported: %+v", ev)
	}
}

func TestDetectorPeg(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	if ev := d.ObservePeg("USDT", 0.9988); len(ev) != 0 {
		t.Fatalf("12bps peg drift fired: %+v", ev)
	}
	ev := d.ObservePeg("USDT", 0.9930)
	if len(ev) != 1 || ev[0].Kind != kindStablecoinPeg || ev[0].Severity != "alert" {
		t.Fatalf("peg break not reported: %+v", ev)
	}
	ev = d.ObservePeg("USDT", 0.9995)
	if len(ev) != 1 || ev[0].Severity != "info" {
		t.Fatalf("peg recovery not reported: %+v", ev)
	}
}

func TestDetectorVenueDownAndRecovery(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	feed(t, d, 16, map[string]float64{})

	var last []Event
	for i := 0; i < 5; i++ {
		last = d.Observe("BTC", snapshot("BTC", map[string]float64{}, map[string]string{"binance": "http 503"}))
	}
	found := false
	for _, e := range last {
		if e.Kind == kindVenueDown && e.Venue == "binance" {
			found = true
		}
	}
	if !found {
		t.Fatalf("5 consecutive failures did not raise venue_down: %+v", last)
	}
	var rec []Event
	for i := 0; i < 1; i++ {
		rec = append(rec, d.Observe("BTC", snapshot("BTC", map[string]float64{}, nil))...)
	}
	found = false
	for _, e := range rec {
		if e.Kind == kindVenueRecover {
			found = true
		}
	}
	if !found {
		t.Fatalf("recovery not reported: %+v", rec)
	}
}
func TestDetectorNoOutageForUnlistedVenue(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	// Bitfinex answers HTTP 500 for pairs it does not list. A venue that never
	// produced a price here is coverage, not an outage: this used to emit one
	// bogus venue_down per symbol per cooldown.
	var ev []Event
	for i := 0; i < 20; i++ {
		ev = append(ev, d.Observe("BTC", snapshot("BTC", map[string]float64{}, map[string]string{"binance": "http 500"}))...)
	}
	for _, e := range ev {
		if e.Kind == kindVenueDown && e.Venue == "binance" {
			t.Fatalf("unlisted venue reported as outage: %+v", e)
		}
	}
}
func TestDetectorOutageIsVenueLevelNotPerSymbol(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	for i := 0; i < 16; i++ {
		d.Observe("BTC", snapshot("BTC", nil, nil))
		d.Observe("ETH", snapshot("ETH", nil, nil))
	}
	var ev []Event
	for i := 0; i < 6; i++ {
		ev = append(ev, d.Observe("BTC", snapshot("BTC", nil, map[string]string{"binance": "http 503"}))...)
		ev = append(ev, d.Observe("ETH", snapshot("ETH", nil, map[string]string{"binance": "http 503"}))...)
	}
	n := 0
	for _, e := range ev {
		if e.Kind == kindVenueDown && e.Venue == "binance" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("venue_down emitted %d times across 2 symbols, want 1", n)
	}
}
func TestPriorDoesNotFireOnFirstPrintsAfterRestart(t *testing.T) {
	priorPath := filepath.Join(t.TempDir(), "baseline.json")
	d1 := newDetector(defaultDetectConfig(), "", priorPath, logNotifier{})
	feed(t, d1, 16, map[string]float64{})
	d1.SavePriors()

	// A fresh detector starts with an empty ring and the saved prior.
	d2 := newDetector(defaultDetectConfig(), "", priorPath, logNotifier{})
	var ev []Event
	for i := 0; i < 2; i++ {
		ev = append(ev, feed(t, d2, 1, map[string]float64{"binance": 300})...)
	}
	if len(ev) != 0 {
		t.Fatalf("prior fired on the first prints after restart: %+v", ev[0])
	}
	// The prior must still catch a real dislocation once a few fresh samples
	// have landed; the guard is about noise, not about going blind.
	for i := 0; i < 5; i++ {
		ev = append(ev, feed(t, d2, 1, map[string]float64{"binance": 300})...)
	}
	found := false
	for _, e := range ev {
		if e.Kind == kindVenueDepeg && e.Venue == "binance" {
			found = true
		}
	}
	if !found {
		t.Fatalf("prior mode went blind: no event over 5 samples at 300bps")
	}
}
func TestDetectorDoesNotAbsorbSustainedDislocation(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	feed(t, d, 20, map[string]float64{})

	ev := feed(t, d, 2, map[string]float64{"binance": 200})
	alerted := false
	for _, e := range ev {
		if e.Kind == kindVenueDepeg && e.Venue == "binance" {
			alerted = true
		}
	}
	if !alerted {
		t.Fatalf("no alert at +200bps: %+v", ev)
	}

	// The price does not come back. A baseline that keeps absorbing the prints
	// would drift up to meet them and then announce a recovery that never
	// happened — this is the regression that was seen live on WLD/HTX.
	for i := 0; i < 40; i++ {
		for _, e := range feed(t, d, 1, map[string]float64{"binance": 230}) {
			if e.Kind == kindVenueNormal {
				t.Fatalf("false recovery while still %.0fbps out: %+v", e.DevBps, e)
			}
			if e.Kind == kindVenueDepeg {
				t.Fatalf("duplicate alert without escalation: %+v", e)
			}
		}
	}
}
func TestPriorModeHandlesConvergenceAndStillClears(t *testing.T) {
	priorPath := filepath.Join(t.TempDir(), "baseline.json")
	d1 := newDetector(defaultDetectConfig(), "", priorPath, logNotifier{})
	// A venue that has been persistently 300bps rich.
	feed(t, d1, 16, map[string]float64{"kraken": 300})
	d1.SavePriors()

	// Restored from that prior: a collapse back to +40bps is convergence, not a
	// new depeg, and must not be mailed as one.
	d2 := newDetector(defaultDetectConfig(), "", priorPath, logNotifier{})
	for i := 0; i < 4; i++ {
		for _, e := range feed(t, d2, 1, map[string]float64{"kraken": 40}) {
			if e.Kind == kindVenueDepeg {
				t.Fatalf("convergence reported as dislocation: %+v", e)
			}
		}
	}

	// A genuine widening against the same prior must still fire...
	alerted := false
	for i := 0; i < 5; i++ {
		for _, e := range feed(t, d2, 1, map[string]float64{"kraken": 700}) {
			if e.Kind == kindVenueDepeg {
				alerted = true
			}
		}
	}
	if !alerted {
		t.Fatal("widening against a restored prior did not alert")
	}

	// ...and the alert must be able to clear once the ring rebuilds. Freezing
	// samples in prior mode would pin the venue to the stale prior forever.
	cleared := false
	for i := 0; i < 25; i++ {
		for _, e := range feed(t, d2, 1, map[string]float64{"kraken": 40}) {
			if e.Kind == kindVenueNormal {
				cleared = true
			}
		}
	}
	if !cleared {
		t.Fatal("alert opened in prior mode never cleared after the ring refilled")
	}
}

// bookSnapshot is snapshot() with a 24h quote volume and a two-sided book of a
// given half-spread, so one test venue can be made thin while the rest stay
// liquid. Venues absent from volume get 0, which means "not reported" and must
// keep the old last-price behaviour.
func bookSnapshot(base string, bps, lastBps, volume map[string]float64, halfSpreadBps float64) comparison {
	c := comparison{Base: base, Quote: "USDT", FetchedAt: time.Now().UnixMilli()}
	for _, v := range testVenues {
		mid := 700 * (1 + bps[v.id]/10000)
		q := ticker{Exchange: v.id, Name: v.name, Kind: v.kind, Quote: "USDT"}
		q.Price = mid * (1 + lastBps[v.id]/10000)
		q.PriceUSDT = q.Price
		q.BidUSDT = mid * (1 - halfSpreadBps/10000)
		q.AskUSDT = mid * (1 + halfSpreadBps/10000)
		q.VolumeQuote = volume[v.id]
		c.Quotes = append(c.Quotes, q)
	}
	c.Summary = summarise(c.Quotes)
	return c
}

func observe(t *testing.T, d *detector, n int, c comparison) []Event {
	t.Helper()
	var out []Event
	for i := 0; i < n; i++ {
		out = append(out, d.Observe(c.Base, c)...)
	}
	return out
}

func findVenueEvent(ev []Event, kind, venue string) *Event {
	for i := range ev {
		if ev[i].Kind == kind && ev[i].Venue == venue {
			return &ev[i]
		}
	}
	return nil
}

// A taker sweeping a thin book moves the last print several percent while the
// quotable mid barely moves. The alert must follow the book, not the print.
func TestThinCEXBookIsJudgedByMidNotLastPrint(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	vol := map[string]float64{"gate": 3000} // $3k/day: one order walks the book
	observe(t, d, 20, bookSnapshot("BTC", nil, nil, vol, 40))

	// Last prints 500bps low after a sweep; the mid stays on consensus.
	ev := observe(t, d, 3, bookSnapshot("BTC", nil, map[string]float64{"gate": -500}, vol, 40))
	if e := findVenueEvent(ev, kindVenueDepeg, "gate"); e != nil {
		t.Fatalf("single print on a thin book fired: %+v", e)
	}
	sig := findSignal(d.Signals("BTC"), "gate")
	if sig == nil {
		t.Fatal("thin venue missing from signals")
	}
	if math.Abs(sig.DevBps) > 1 {
		t.Fatalf("thin venue dev = %.1fbps, want ~0 from the mid", sig.DevBps)
	}
}

// A genuine move on a thin book is worth reporting, but never at alert level:
// the mailbox is reserved for venues where the print can be traded at size.
func TestThinCEXNeverEscalatesToAlert(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	vol := map[string]float64{"gate": 3000}
	observe(t, d, 20, bookSnapshot("BTC", nil, nil, vol, 40))

	ev := observe(t, d, 3, bookSnapshot("BTC",
		map[string]float64{"gate": -600}, map[string]float64{"gate": -600}, vol, 40))
	e := findVenueEvent(ev, kindVenueDepeg, "gate")
	if e == nil {
		t.Fatal("thin dislocation produced no event at all")
	}
	if e.Severity != "watch" {
		t.Fatalf("severity = %q, want watch on a thin book", e.Severity)
	}
	if e.ThresholdBps < 120 {
		t.Fatalf("threshold = %.0fbps, want the thin floor of 120", e.ThresholdBps)
	}
}

// The gate must not blind the detector on venues that can actually trade: the
// same move on a liquid book still has to mail an alert.
func TestLiquidCEXStillAlerts(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	vol := map[string]float64{"gate": 5_000_000}
	observe(t, d, 20, bookSnapshot("BTC", nil, nil, vol, 40))

	ev := observe(t, d, 3, bookSnapshot("BTC",
		map[string]float64{"gate": -600}, nil, vol, 40))
	e := findVenueEvent(ev, kindVenueDepeg, "gate")
	if e == nil {
		t.Fatal("liquid dislocation produced no event")
	}
	if e.Severity != "alert" {
		t.Fatalf("severity = %q, want alert on a liquid book", e.Severity)
	}
}

func findSignal(sigs []Signal, venue string) *Signal {
	for i := range sigs {
		if sigs[i].Venue == venue {
			return &sigs[i]
		}
	}
	return nil
}

// A DEX print that failed the same-token USD-pool cross-check must never be
// alerted on — that is how a stale upstream snapshot of a SOL-quoted pool
// looked like an 1100bps BTC depeg — but it must stay visible.
func TestSuspectDEXPrintNeverAlertsAndStaysVisible(t *testing.T) {
	d := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	feed(t, d, 20, map[string]float64{})

	for i := 0; i < 5; i++ {
		c := snapshot("BTC", map[string]float64{"uniswap-eth": -1100}, nil)
		c.Quotes[len(c.Quotes)-1].Suspect = true
		for _, e := range d.Observe("BTC", c) {
			if e.Kind == kindVenueDepeg && e.Venue == "uniswap-eth" {
				t.Fatalf("suspect print alerted: %+v", e)
			}
		}
	}
	found := false
	for _, s := range d.Signals("BTC") {
		if s.Venue != "uniswap-eth" {
			continue
		}
		found = true
		if !s.Suspect || s.State != "suspect" {
			t.Fatalf("signal = %+v, want state=suspect", s)
		}
		if s.Price <= 0 {
			t.Fatalf("signal lost its display price: %+v", s)
		}
	}
	if !found {
		t.Fatal("suspect venue vanished from /api/signals")
	}

	// Control: the same print without the flags still alerts, so the filter
	// cannot silently disable DEX alerting.
	d2 := newDetector(defaultDetectConfig(), "", "", logNotifier{})
	ev := feed(t, d2, 20, map[string]float64{})
	ev = append(ev, feed(t, d2, 2, map[string]float64{"uniswap-eth": -1100})...)
	alerted := false
	for _, e := range ev {
		if e.Kind == kindVenueDepeg && e.Venue == "uniswap-eth" {
			alerted = true
		}
	}
	if !alerted {
		t.Fatal("control print did not alert; the suspect gate must not swallow real events")
	}
}
