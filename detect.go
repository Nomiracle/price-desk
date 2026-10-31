package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The detector answers one question on a schedule, rather than only when a
// browser happens to be open: "is any venue's price, measured against the
// centralised consensus, further from its own recent behaviour than noise can
// explain?" It is deliberately price-only and alert-only: no execution path.

const (
	kindVenueDepeg    = "venue_depeg"
	kindVenueNormal   = "venue_normal"
	kindDispersion    = "dispersion"
	kindStablecoinPeg = "stablecoin_peg"
	kindVenueDown     = "venue_down"
	kindVenueRecover  = "venue_recovered"
)

type detectConfig struct {
	Window             int           // samples kept per symbol
	MinSamples         int           // samples before a baseline is trusted
	K                  float64       // sigma multiplier (MAD units)
	Confirm            int           // consecutive anomalous samples required
	Cooldown           time.Duration // quiet period per symbol+venue
	CEXFloorBps        float64       // minimum threshold, centralised venue
	DEXFloorBps        float64       // minimum threshold, on-chain pool
	CEXAbsFloorBps     float64       // ignore small absolute deviations
	DEXAbsFloorBps     float64
	DispersionFloorBps float64
	DispersionRatio    float64
	PegFloorBps        float64
	DownAfter          int // consecutive feed errors before "venue_down"
	PriorTTL           time.Duration
}

// applyDetectEnv lets the operator tune sensitivity without a rebuild. Values
// are read once at startup and logged, so the effective thresholds are never a
// guess.
func applyDetectEnv(cfg *detectConfig) {
	setF := func(key string, dst *float64) {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return
		}
		if fv, err := strconv.ParseFloat(v, 64); err == nil && fv > 0 {
			*dst = fv
		} else {
			log.Printf("monitor: ignoring %s=%q (not a positive number)", key, v)
		}
	}
	setF("PRICE_ALERT_CEX_BPS", &cfg.CEXFloorBps)
	setF("PRICE_ALERT_DEX_BPS", &cfg.DEXFloorBps)
	setF("PRICE_ALERT_PEG_BPS", &cfg.PegFloorBps)
	setF("PRICE_ALERT_CEX_ABS_BPS", &cfg.CEXAbsFloorBps)
	setF("PRICE_ALERT_DEX_ABS_BPS", &cfg.DEXAbsFloorBps)
	if v := strings.TrimSpace(os.Getenv("PRICE_ALERT_CONFIRM")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 20 {
			cfg.Confirm = n
		} else {
			log.Printf("monitor: ignoring PRICE_ALERT_CONFIRM=%q", v)
		}
	}
}

func defaultDetectConfig() detectConfig {
	return detectConfig{
		Window:             180,
		MinSamples:         15,
		K:                  4.0,
		Confirm:            2,
		Cooldown:           30 * time.Minute,
		CEXFloorBps:        45,
		DEXFloorBps:        150,
		CEXAbsFloorBps:     25,
		DEXAbsFloorBps:     60,
		DispersionFloorBps: 150,
		DispersionRatio:    2.5,
		PegFloorBps:        50,
		DownAfter:          5,
		PriorTTL:           60 * time.Minute,
	}
}

type Event struct {
	ID           string  `json:"id"`
	At           int64   `json:"at"` // unix ms
	Kind         string  `json:"kind"`
	Symbol       string  `json:"symbol"`
	Venue        string  `json:"venue,omitempty"`
	VenueName    string  `json:"venue_name,omitempty"`
	MarketKind   string  `json:"market_kind,omitempty"` // cex | dex
	Price        float64 `json:"price,omitempty"`
	Consensus    float64 `json:"consensus,omitempty"`
	DevBps       float64 `json:"dev_bps,omitempty"`
	BaselineBps  float64 `json:"baseline_bps,omitempty"`
	MADBps       float64 `json:"mad_bps,omitempty"`
	ThresholdBps float64 `json:"threshold_bps,omitempty"`
	Severity     string  `json:"severity"` // watch | alert | info
	Note         string  `json:"note,omitempty"`
	Samples      int     `json:"samples,omitempty"`
}

// Signal is the live per-venue view used by /api/signals.
type Signal struct {
	Symbol       string  `json:"symbol"`
	Venue        string  `json:"venue"`
	VenueName    string  `json:"venue_name"`
	MarketKind   string  `json:"market_kind"`
	Price        float64 `json:"price"`
	DevBps       float64 `json:"dev_bps"`
	BaselineBps  float64 `json:"baseline_bps"`
	MADBps       float64 `json:"mad_bps"`
	ThresholdBps float64 `json:"threshold_bps"`
	Samples      int     `json:"samples"`
	State        string  `json:"state"` // ok | watch | alert | warmup
}

type sample struct {
	at   int64
	disp float64
	dev  map[string]float64
	// frozen marks venues that were already alerted when this sample was
	// taken. Their prints must not enter the baseline: otherwise a sustained
	// dislocation slowly becomes "normal" and the detector reports a recovery
	// that never happened.
	frozen map[string]bool
	ok     int
}

type alertState struct {
	lastAt  int64
	lastDev float64
	alerted bool
}

type symbolState struct {
	samples   []sample
	prices    map[string]float64
	hitStreak map[string]int
	alert     map[string]alertState
	errStreak map[string]int
	down      map[string]bool
	kind      map[string]string
	name      map[string]string
}

type prior struct {
	Med     float64 `json:"med"`
	MAD     float64 `json:"mad"`
	N       int     `json:"n"`
	SavedAt int64   `json:"saved_at"`
}

type priorFile struct {
	Version int                          `json:"version"`
	SavedAt int64                        `json:"saved_at"`
	Symbols map[string]map[string]prior  `json:"symbols"`
}

type detector struct {
	mu          sync.Mutex
	cfg         detectConfig
	series      map[string]*symbolState
	events      []Event
	maxEvents   int
	totalEvents int64
	priors      priorFile
	eventsPath  string
	priorPath   string
	notifier    notifier
	// stablecoin pegs, keyed by symbol
	pegSamples map[string][]float64
	pegAlert   map[string]alertState
	// feed outages are a venue-level fact; the per-symbol state machine would
	// otherwise emit one message per symbol for the same outage
	feedAlert map[string]int64
}

func newDetector(cfg detectConfig, eventsPath, priorPath string, n notifier) *detector {
	d := &detector{
		cfg:        cfg,
		series:     map[string]*symbolState{},
		maxEvents:  500,
		eventsPath: eventsPath,
		priorPath:  priorPath,
		notifier:   n,
		pegSamples: map[string][]float64{},
		pegAlert:   map[string]alertState{},
		feedAlert:  map[string]int64{},
		priors:     priorFile{Version: 1, Symbols: map[string]map[string]prior{}},
	}
	d.loadPriors()
	d.loadEvents()
	return d
}

func (d *detector) state(base string) *symbolState {
	s, ok := d.series[base]
	if !ok {
		s = &symbolState{
			prices:    map[string]float64{},
			hitStreak: map[string]int{},
			alert:     map[string]alertState{},
			errStreak: map[string]int{},
			down:      map[string]bool{},
			kind:      map[string]string{},
			name:      map[string]string{},
		}
		d.series[base] = s
	}
	return s
}

// Observe folds one comparison snapshot into the detector and returns the
// events it produced (already persisted and queued for notification).
func (d *detector) Observe(base string, c comparison) []Event {
	now := time.Now().UnixMilli()

	var cex []float64
	for i := range c.Quotes {
		q := &c.Quotes[i]
		if q.Kind == "cex" && q.Error == "" && !q.NotListed && q.PriceUSDT > 0 {
			cex = append(cex, q.PriceUSDT)
		}
	}
	if len(cex) < 4 {
		// Too few centralised books to define a consensus; anything measured
		// against two venues would be noise.
		return nil
	}
	consensus := median(cex)

	d.mu.Lock()
	s := d.state(base)
	sm := sample{at: now, dev: map[string]float64{}, ok: len(cex)}
	for i := range c.Quotes {
		q := &c.Quotes[i]
		s.name[q.Exchange] = q.Name
		s.kind[q.Exchange] = q.Kind
		if q.NotListed {
			// A market the venue does not list is coverage, not an outage.
			s.errStreak[q.Exchange] = 0
		} else if q.Error != "" {
			s.errStreak[q.Exchange]++
		} else {
			s.errStreak[q.Exchange] = 0
		}
		if q.Error != "" || q.NotListed || q.LowLiquidity || q.PriceUSDT <= 0 {
			continue
		}
		s.prices[q.Exchange] = q.PriceUSDT
		sm.dev[q.Exchange] = (q.PriceUSDT/consensus - 1) * 10000
		// Freezing is only meaningful once the ring itself is the baseline.
		// In prior mode the ring is empty, so freezing every new print would
		// pin the venue to a stale prior forever and the alert could never
		// clear.
		if st, alerted := s.alert[q.Exchange]; alerted && st.alerted && len(s.samples) > d.cfg.MinSamples {
			if sm.frozen == nil {
				sm.frozen = map[string]bool{}
			}
			sm.frozen[q.Exchange] = true
		}
	}
	sm.disp = c.Summary.DispersionBps
	s.samples = append(s.samples, sm)
	if len(s.samples) > d.cfg.Window {
		s.samples = s.samples[len(s.samples)-d.cfg.Window:]
	}

	var out []Event
	out = append(out, d.analyseSymbol(base, s, c, consensus, now)...)
	out = append(out, d.analyseFeeds(base, s, c, now)...)
	d.mu.Unlock()

	d.dispatch(out)
	return out
}

func (d *detector) analyseSymbol(base string, s *symbolState, c comparison, consensus float64, now int64) []Event {
	if len(s.samples) < 3 {
		// After a restart the baseline comes from the saved prior, but the
		// first fresh prints can still be a burst of stale/one-off quotes. Let
		// three samples land before anything is judged against that prior.
		return nil
	}
	cur := s.samples[len(s.samples)-1]
	var out []Event

	// Per-venue deviation against the venue's own recent deviation. A venue
	// that persistently trades 20bps above consensus must not be flagged for
	// doing so; only a change from that behaviour is an event.
	for venue, dev := range cur.dev {
		floor := d.cfg.CEXFloorBps
		absFloor := d.cfg.CEXAbsFloorBps
		if s.kind[venue] == "dex" {
			floor = d.cfg.DEXFloorBps
			absFloor = d.cfg.DEXAbsFloorBps
		}
		med, mad, n, ok, restored := d.baseline(base, s, venue)
		if !ok {
			// Baseline still warming up: observe, never alert.
			continue
		}
		thr := math.Max(d.cfg.K*mad, floor)
		need := d.cfg.Confirm
		if restored {
			// The ring is empty and we are trusting a saved prior whose MAD
			// came from a short warm-up window. Raise the bar until the ring
			// refills, otherwise every thin venue's normal wobble looks like a
			// 45bps event after each restart.
			thr = math.Max(thr, floor*2)
			need = d.cfg.Confirm + 1
		}
		delta := math.Abs(dev - med)
		anomalous := delta > thr && math.Abs(dev) > absFloor
		if restored && math.Abs(med) > 5 && math.Abs(dev) < math.Abs(med) {
			// Breathing with a saved prior: a premium collapsing back toward
			// the pack is convergence, not a dislocation. A fresh process has
			// no alert state to resolve, so mailing "depeg" here is wrong.
			anomalous = false
		}
		if anomalous {
			s.hitStreak[venue]++
		} else {
			s.hitStreak[venue] = 0
		}

		if anomalous && s.hitStreak[venue] >= need && d.allow(s, venue, now, dev) {
			sev := "watch"
			if delta > 2*thr {
				sev = "alert"
			}
			out = append(out, Event{
				At: now, Kind: kindVenueDepeg, Symbol: base, Venue: venue,
				VenueName: s.name[venue], MarketKind: s.kind[venue],
				Price: round8(priceOf(c, venue)), Consensus: round8(consensus),
				DevBps: round1(dev), BaselineBps: round1(med), MADBps: round1(mad),
				ThresholdBps: round1(thr), Severity: sev, Samples: n,
			})
			s.alert[venue] = alertState{lastAt: now, lastDev: dev, alerted: true}
		}
		// A flagged venue that comes back inside its band clears the state, so
		// the next dislocation is reported instead of being swallowed by the
		// cooldown.
		if !anomalous && s.hitStreak[venue] == 0 {
			if st, ok := s.alert[venue]; ok && st.alerted {
				out = append(out, Event{
					At: now, Kind: kindVenueNormal, Symbol: base, Venue: venue,
					VenueName: s.name[venue], MarketKind: s.kind[venue],
					Price: round8(priceOf(c, venue)), Consensus: round8(consensus),
					DevBps: round1(dev), BaselineBps: round1(med),
					ThresholdBps: round1(thr), Severity: "info", Samples: n,
					Note: "回到基线区间",
				})
				s.alert[venue] = alertState{lastAt: now, lastDev: dev, alerted: false}
			}
		}
	}

	// Whole-symbol dispersion: catches "the books disagree" even when no single
	// venue is far enough out on its own baseline.
	window := []float64{}
	for _, x := range s.samples {
		if x.disp > 0 {
			window = append(window, x.disp)
		}
	}
	baseDisp := median(window)
	if len(window) >= d.cfg.MinSamples && cur.disp > math.Max(d.cfg.DispersionRatio*baseDisp, d.cfg.DispersionFloorBps) {
		s.hitStreak["__disp"]++
	} else {
		s.hitStreak["__disp"] = 0
	}
	if s.hitStreak["__disp"] >= d.cfg.Confirm && d.allow(s, "__disp", now, cur.disp) {
		out = append(out, Event{
			At: now, Kind: kindDispersion, Symbol: base,
			Consensus: round8(consensus), DevBps: round1(cur.disp),
			BaselineBps: round1(baseDisp), ThresholdBps: round1(math.Max(d.cfg.DispersionFloorBps, d.cfg.DispersionRatio*baseDisp)),
			Severity: "watch", Samples: len(window),
			Note: fmt.Sprintf("%d 个场所报价离散度异常", cur.ok),
		})
		s.alert["__disp"] = alertState{lastAt: now, lastDev: cur.disp, alerted: true}
	}
	return out
}

// allowFeed rate-limits venue-level outage messages across symbols.
func (d *detector) allowFeed(key string, now int64) bool {
	if last, ok := d.feedAlert[key]; ok && now-last < int64(d.cfg.Cooldown/time.Millisecond) {
		return false
	}
	d.feedAlert[key] = now
	return true
}

// allow decides whether a still-anomalous venue deserves another message.
// While an alert stands, only an escalation (1.6x the deviation that opened
// it) is new information; re-sending the same level every cooldown would turn
// a persistent dislocation into a timer. Once the venue has returned inside
// its band, the cooldown keeps a flapping venue from re-alerting immediately.
func (d *detector) allow(s *symbolState, key string, now int64, dev float64) bool {
	st, ok := s.alert[key]
	if !ok {
		return true
	}
	grown := math.Abs(dev) > math.Abs(st.lastDev)*1.6
	if st.alerted {
		return grown
	}
	return grown || now-st.lastAt >= int64(d.cfg.Cooldown/time.Millisecond)
}

// baseline returns the venue's median/MAD deviation from the ring, falling
// back to a persisted prior while the ring refills after a restart.
func (d *detector) baseline(base string, s *symbolState, venue string) (med, mad float64, n int, ok bool, restored bool) {
	col := make([]float64, 0, len(s.samples))
	for _, x := range s.samples {
		if x.frozen != nil && x.frozen[venue] {
			continue
		}
		if v, has := x.dev[venue]; has {
			col = append(col, v)
		}
	}
	if len(col) >= d.cfg.MinSamples {
		m, sd := medianMAD(col)
		return m, sd, len(col), true, false
	}
	if p, has := d.priors.Symbols[base][venue]; has {
		age := time.Duration(time.Now().UnixMilli()-p.SavedAt) * time.Millisecond
		if p.N >= d.cfg.MinSamples && age < d.cfg.PriorTTL {
			return p.Med, p.MAD, p.N, true, true
		}
	}
	return 0, 0, len(col), false, false
}

func (d *detector) analyseFeeds(base string, s *symbolState, c comparison, now int64) []Event {
	var out []Event
	for i := range c.Quotes {
		q := &c.Quotes[i]
		// A venue can only be "down" for a market it has actually priced
		// before. Exchanges that answer HTTP 500 for pairs they simply do not
		// list would otherwise generate one bogus outage per symbol, forever.
		okSeen := 0
		for _, x := range s.samples {
			if _, has := x.dev[q.Exchange]; has {
				okSeen++
				if okSeen >= 3 {
					break
				}
			}
		}
		if okSeen < 3 {
			s.errStreak[q.Exchange] = 0
			continue
		}
		if q.Error == "" && !q.NotListed {
			if s.down[q.Exchange] {
				if d.allowFeed("up:"+q.Exchange, now) {
					out = append(out, Event{
						At: now, Kind: kindVenueRecover, Symbol: base, Venue: q.Exchange,
						VenueName: q.Name, MarketKind: q.Kind, Severity: "info",
						Note: "行情恢复",
					})
				}
				s.down[q.Exchange] = false
				s.errStreak[q.Exchange] = 0
			}
			continue
		}
		if q.NotListed {
			s.errStreak[q.Exchange] = 0
			continue
		}
		if s.errStreak[q.Exchange] >= d.cfg.DownAfter && !s.down[q.Exchange] {
			if d.allowFeed("down:"+q.Exchange, now) {
				out = append(out, Event{
					At: now, Kind: kindVenueDown, Symbol: base, Venue: q.Exchange,
					VenueName: q.Name, MarketKind: q.Kind, Severity: "watch",
					Note: fmt.Sprintf("连续 %d 次行情失败（%s）", s.errStreak[q.Exchange], base),
				})
			}
			s.down[q.Exchange] = true
		}
	}
	return out
}

// ObservePeg folds a stablecoin/USD print (1.0 = peg) into the peg detector.
func (d *detector) ObservePeg(symbol string, price float64) []Event {
	if price <= 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	bps := (price - 1) * 10000

	d.mu.Lock()
	arr := append(d.pegSamples[symbol], bps)
	if len(arr) > d.cfg.Window {
		arr = arr[len(arr)-d.cfg.Window:]
	}
	d.pegSamples[symbol] = arr
	var out []Event
	st := d.pegAlert[symbol]
	if math.Abs(bps) > d.cfg.PegFloorBps {
		// Peg deviations are judged in absolute terms, not against a rolling
		// baseline: the peg is 1.0 by definition, and USDT at 0.995 is an
		// event regardless of what it did yesterday.
		if now-st.lastAt >= int64(d.cfg.Cooldown/time.Millisecond) || math.Abs(bps) > math.Abs(st.lastDev)*1.4 {
			out = append(out, Event{
				At: now, Kind: kindStablecoinPeg, Symbol: symbol,
				Price: round8(price), DevBps: round1(bps), Severity: "alert",
				ThresholdBps: d.cfg.PegFloorBps, Samples: len(arr),
				Note: "稳定币对美元脱锚",
			})
			d.pegAlert[symbol] = alertState{lastAt: now, lastDev: bps, alerted: true}
		}
	} else if st.alerted {
		out = append(out, Event{
			At: now, Kind: kindStablecoinPeg, Symbol: symbol,
			Price: round8(price), DevBps: round1(bps), Severity: "info",
			ThresholdBps: d.cfg.PegFloorBps, Note: "回到锚定区间",
		})
		d.pegAlert[symbol] = alertState{lastAt: now, lastDev: bps, alerted: false}
	}
	d.mu.Unlock()

	d.dispatch(out)
	return out
}

func (d *detector) dispatch(events []Event) {
	for _, e := range events {
		if e.Kind == kindVenueDepeg && e.Severity == "info" {
			// warmup notices are API-only, never alerts
			d.store(e, false)
			continue
		}
		d.store(e, true)
	}
}

func (d *detector) store(e Event, notify bool) {
	if e.ID == "" {
		e.ID = newEventID()
	}
	d.mu.Lock()
	d.events = append(d.events, e)
	if len(d.events) > d.maxEvents {
		d.events = d.events[len(d.events)-d.maxEvents:]
	}
	d.totalEvents++
	path := d.eventsPath
	d.mu.Unlock()

	if path != "" {
		if line, err := json.Marshal(e); err == nil {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				_, _ = f.Write(append(line, '\n'))
				_ = f.Close()
			}
		}
	}
	if notify && d.notifier != nil {
		d.notifier.Notify(e)
	}
}

func newEventID() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (d *detector) Events(limit int, since int64) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// newest first
	out := make([]Event, 0, limit)
	for i := len(d.events) - 1; i >= 0 && len(out) < limit; i-- {
		if d.events[i].At >= since {
			out = append(out, d.events[i])
		}
	}
	return out
}

func (d *detector) Signals(base string) []Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.series[base]
	if s == nil || len(s.samples) == 0 {
		return []Signal{}
	}
	cur := s.samples[len(s.samples)-1]
	out := make([]Signal, 0, len(cur.dev))
	for venue, dev := range cur.dev {
		floor := d.cfg.CEXFloorBps
		if s.kind[venue] == "dex" {
			floor = d.cfg.DEXFloorBps
		}
		med, mad, n, ok, _ := d.baseline(base, s, venue)
		sig := Signal{
			Symbol: base, Venue: venue, VenueName: s.name[venue],
			MarketKind: s.kind[venue], Price: round8(priceOfSample(s, venue)),
			DevBps: round1(dev), Samples: n, State: "warmup",
		}
		if ok {
			thr := math.Max(d.cfg.K*mad, floor)
			sig.BaselineBps, sig.MADBps, sig.ThresholdBps = round1(med), round1(mad), round1(thr)
			sig.State = "ok"
			delta := math.Abs(dev - med)
			if delta > thr {
				sig.State = "watch"
				if delta > 2*thr {
					sig.State = "alert"
				}
			}
		}
		out = append(out, sig)
	}
	return out
}

// priceOfSample is only used for the per-venue display price in signals; the
// price itself comes from the latest comparison, kept in the sample's dev map
// implicitly. We re-derive it from consensus-free data when available.
func priceOfSample(s *symbolState, venue string) float64 { return s.prices[venue] }

// monitorStatus is the collector's self-report for /api/status.
type monitorStatus struct {
	Enabled      bool   `json:"enabled"`
	IntervalS    int    `json:"interval_s"`
	ReadySymbols int    `json:"ready_symbols"`
	TotalSymbols int    `json:"total_symbols"`
	Warmup       bool   `json:"warmup"`
	AlertsOpen   int    `json:"alerts_open"`
	Notifier     string `json:"notifier"`
	Cycles       int64  `json:"cycles"`
	LastCycleAt  int64  `json:"last_cycle_at"`
	LastCycleMS  int64  `json:"last_cycle_ms"`
}

func (d *detector) Status(intervalS int) monitorStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	ready := 0
	for _, s := range d.series {
		if len(s.samples) >= d.cfg.MinSamples {
			ready++
		}
	}
	open := 0
	for _, s := range d.series {
		for _, a := range s.alert {
			if a.alerted {
				open++
			}
		}
	}
	for _, a := range d.pegAlert {
		if a.alerted {
			open++
		}
	}
	name := "log"
	if d.notifier != nil {
		name = d.notifier.Name()
	}
	return monitorStatus{
		Enabled:      true,
		IntervalS:    intervalS,
		ReadySymbols: ready,
		TotalSymbols: len(symbolCatalog),
		Warmup:       ready < len(symbolCatalog),
		AlertsOpen:   open,
		Notifier:     name,
	}
}

// Warming reports whether any symbol still lacks a trustworthy baseline, so
// the collector can sample faster until the detector is armed.
func (d *detector) Warming() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.series) < len(symbolCatalog) {
		return true
	}
	for _, s := range d.series {
		if len(s.samples) < d.cfg.MinSamples {
			return true
		}
	}
	return false
}

func (d *detector) savePriorsLocked() {
	snapshot := priorFile{Version: 1, SavedAt: time.Now().UnixMilli(), Symbols: map[string]map[string]prior{}}
	for base, s := range d.series {
		if len(s.samples) < d.cfg.MinSamples {
			continue
		}
		m := map[string]prior{}
		for venue := range s.samples[len(s.samples)-1].dev {
			med, mad, n, ok, _ := d.baseline(base, s, venue)
			if ok {
				m[venue] = prior{Med: med, MAD: mad, N: n, SavedAt: time.Now().UnixMilli()}
			}
		}
		if len(m) > 0 {
			snapshot.Symbols[base] = m
		}
	}
	if len(snapshot.Symbols) == 0 {
		return
	}
	d.priors = snapshot
	if d.priorPath == "" {
		return
	}
	if b, err := json.Marshal(snapshot); err == nil {
		tmp := d.priorPath + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err == nil {
			_ = os.Rename(tmp, d.priorPath)
		}
	}
}

func (d *detector) SavePriors() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.savePriorsLocked()
}

func (d *detector) loadPriors() {
	if d.priorPath == "" {
		return
	}
	b, err := os.ReadFile(d.priorPath)
	if err != nil {
		return
	}
	var pf priorFile
	if json.Unmarshal(b, &pf) != nil || pf.Version != 1 {
		return
	}
	d.priors = pf
}

func (d *detector) loadEvents() {
	if d.eventsPath == "" {
		return
	}
	b, err := os.ReadFile(d.eventsPath)
	if err != nil {
		return
	}
	lines := splitLines(b)
	if len(lines) > d.maxEvents {
		lines = lines[len(lines)-d.maxEvents:]
	}
	for _, line := range lines {
		var e Event
		if json.Unmarshal(line, &e) == nil && e.ID != "" {
			d.events = append(d.events, e)
		}
	}
	d.totalEvents = int64(len(d.events))
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func priceOf(c comparison, venue string) float64 {
	for i := range c.Quotes {
		if c.Quotes[i].Exchange == venue {
			return c.Quotes[i].PriceUSDT
		}
	}
	return 0
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round8(v float64) float64 { return math.Round(v*1e8) / 1e8 }
