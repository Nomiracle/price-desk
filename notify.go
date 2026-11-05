package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// notifier is deliberately tiny: alerts are informational, and a delivery
// failure must never stall the collector.
type notifier interface {
	Name() string
	Notify(Event)
}

type logNotifier struct{}

func (logNotifier) Name() string { return "log" }

func (logNotifier) Notify(e Event) {
	switch e.Kind {
	case kindVenueDepeg, kindVenueNormal:
		log.Printf("[%s] %s %s/%s px=%.8g dev=%.1fbps base=%.1fbps thr=%.1fbps n=%d",
			strings.ToUpper(e.Severity), e.Kind, e.Symbol, e.Venue, e.Price, e.DevBps, e.BaselineBps, e.ThresholdBps, e.Samples)
	case kindDispersion:
		log.Printf("[%s] %s %s disp=%.1fbps base=%.1fbps thr=%.1fbps",
			strings.ToUpper(e.Severity), e.Kind, e.Symbol, e.DevBps, e.BaselineBps, e.ThresholdBps)
	default:
		log.Printf("[%s] %s %s/%s px=%.8g dev=%.1fbps n=%d %s",
			strings.ToUpper(e.Severity), e.Kind, e.Symbol, e.Venue, e.Price, e.DevBps, e.Samples, e.Note)
	}
}

type telegramNotifier struct {
	token string
	chat  string
	c     *http.Client
}

func (t *telegramNotifier) Name() string { return "telegram" }

func (t *telegramNotifier) Notify(e Event) {
	go func() {
		text := formatAlert(e)
		for attempt := 0; attempt < 2; attempt++ {
			if err := t.send(text); err == nil {
				return
			} else if attempt == 1 {
				log.Printf("telegram alert delivery failed: %v", err)
			}
			time.Sleep(time.Second)
		}
	}()
}

func (t *telegramNotifier) send(text string) error {
	form := url.Values{}
	form.Set("chat_id", t.chat)
	form.Set("text", text)
	form.Set("disable_web_page_preview", "true")
	req, err := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+t.token+"/sendMessage",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if !body.OK {
		return fmt.Errorf("telegram: HTTP %d %s", resp.StatusCode, body.Description)
	}
	return nil
}

func formatAlert(e Event) string {
	ts := time.UnixMilli(e.At).UTC().Format("01-02 15:04 UTC")
	switch e.Kind {
	case kindVenueDepeg:
		head := "⚠️ 脱锚"
		if e.Severity == "alert" {
			head = "🚨 脱锚告警"
		}
		where := e.Symbol + " · " + e.VenueName
		if e.Market != "" {
			where += " · " + e.Market
		}
		if e.Pool != "" {
			where += "\n池 " + shortPool(e.Pool)
		}
		return fmt.Sprintf("%s  %s\n现价 %.8g / CEX 共识 %.8g\n偏离 %+.1f bps（基线 %+.1f，阈值 %.1f）\n观测 %d 样本 · %s",
			head, where, e.Price, e.Consensus, e.DevBps, e.BaselineBps, e.ThresholdBps, e.Samples, ts)
	case kindVenueNormal:
		return fmt.Sprintf("✅ 回归  %s · %s\n偏离 %+.1f bps，回到基线区间（%.1f）\n%s",
			e.Symbol, e.VenueName, e.DevBps, e.BaselineBps, ts)
	case kindDispersion:
		return fmt.Sprintf("⚠️ 报价离散  %s\n离散度 %.1f bps（基线 %.1f，阈值 %.1f）\n%s",
			e.Symbol, e.DevBps, e.BaselineBps, e.ThresholdBps, ts)
	case kindStablecoinPeg:
		if e.Severity == "info" {
			return fmt.Sprintf("✅ 回锚  %s/USD %.6f（%+.1f bps）\n%s", e.Symbol, e.Price, e.DevBps, ts)
		}
		return fmt.Sprintf("🚨 稳定币脱锚  %s/USD %.6f\n偏离 %+.1f bps（阈值 %.1f）\n%s",
			e.Symbol, e.Price, e.DevBps, e.ThresholdBps, ts)
	case kindVenueDown:
		return fmt.Sprintf("⚠️ 行情中断  %s · %s\n%s", e.Symbol, e.VenueName, e.Note)
	case kindVenueRecover:
		return fmt.Sprintf("✅ 行情恢复  %s · %s\n%s", e.Symbol, e.VenueName, ts)
	}
	return fmt.Sprintf("%s %s %s", e.Kind, e.Symbol, e.Venue)
}

// multiNotifier fans one event out to every configured channel. Delivery is
// never invented: with no channel the service reports notifier=log instead of
// pretending to push.
type multiNotifier struct{ list []notifier }

func (m *multiNotifier) Name() string {
	var names []string
	for _, n := range m.list {
		if n.Name() != "log" {
			names = append(names, n.Name())
		}
	}
	if len(names) == 0 {
		return "log"
	}
	return strings.Join(names, "+")
}

func (m *multiNotifier) Notify(e Event) {
	for _, n := range m.list {
		n.Notify(e)
	}
}

func newTelegramFromEnv() notifier {
	token := strings.TrimSpace(os.Getenv("PRICE_ALERT_TELEGRAM_TOKEN"))
	chat := strings.TrimSpace(os.Getenv("PRICE_ALERT_TELEGRAM_CHAT"))
	if token == "" || chat == "" {
		return nil
	}
	log.Printf("alerts: telegram chat %s", chat)
	return &telegramNotifier{token: token, chat: chat, c: &http.Client{Timeout: 8 * time.Second}}
}

func newNotifier() notifier {
	// The journal line is always kept: it is the only record that survives a
	// mail provider outage.
	list := []notifier{logNotifier{}}
	if tg := newTelegramFromEnv(); tg != nil {
		list = append(list, tg)
	}
	if em := newEmailFromEnv(); em != nil {
		list = append(list, em)
	}
	if len(list) == 1 {
		log.Printf("alerts: journal only (set a Telegram or email channel to push)")
		return list[0]
	}
	return &multiNotifier{list: list}
}
