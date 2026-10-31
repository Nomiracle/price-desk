package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Email is the second delivery channel. Two transports cover every realistic
// "online solution": any provider's SMTP endpoint (QQ/Gmail/Outlook/Mailgun/
// SendGrid/Brevo/Amazon SES) and Resend's HTTP API, which needs no SMTP
// settings at all. Both are stdlib-only.

type emailConfig struct {
	to      []string
	from    string
	sendAll bool
	// minBps is the magnitude gate for the mailbox. Detection stays sensitive
	// (the page shows watch-level); email is for "big enough to act on".
	minBps float64

	// SMTP transport
	host string
	port int
	user string
	pass string
	tls  string // implicit | starttls | none

	// Resend transport
	resendKey  string
	resendFrom string
}

type emailNotifier struct {
	cfg emailConfig
	c   *http.Client

	// mailed remembers which openings actually went out, so a resolution is
	// only reported for something the mailbox was told about. Without this,
	// every watch-level wobble sends a recovery email for an alert that never
	// existed — 31 of the first 36 messages were exactly that.
	mu     sync.Mutex
	mailed map[string]int64
}

func (n *emailNotifier) Name() string { return "email" }

// wanted keeps the mailbox for things a human should act on: email is slower
// and easier to lose in a spam folder than Telegram, so watch-level samples
// stay in the journal/UI, while alerts and their resolutions are mailed.
func (n *emailNotifier) wanted(e Event) bool {
	if n.cfg.sendAll {
		return true
	}
	key := e.Symbol + "|" + e.Venue
	switch e.Kind {
	case kindVenueNormal:
		// Only close out an alert the mailbox actually saw.
		return n.takeMailed(key)
	case kindVenueRecover:
		return n.takeMailed("down|" + e.Venue)
	case kindVenueDown:
		n.markMailed("down|" + e.Venue)
		return true
	case kindStablecoinPeg:
		if e.Severity == "alert" {
			n.markMailed(key)
			return true
		}
		return n.takeMailed(key)
	}
	if e.Severity != "alert" {
		return false
	}
	if math.Abs(e.DevBps) < n.cfg.minBps {
		return false
	}
	n.markMailed(key)
	return true
}

// markMailed records an opening event, pruning anything older than a day so
// the map cannot grow without bound.
func (n *emailNotifier) markMailed(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.mailed == nil {
		n.mailed = map[string]int64{}
	}
	now := time.Now().UnixMilli()
	for k, at := range n.mailed {
		if now-at > 24*60*60*1000 {
			delete(n.mailed, k)
		}
	}
	n.mailed[key] = now
}

// takeMailed reports whether an opening was mailed and consumes it, so one
// alert cannot produce a stream of resolutions.
func (n *emailNotifier) takeMailed(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.mailed[key]
	if ok {
		delete(n.mailed, key)
	}
	return ok
}

func (n *emailNotifier) Notify(e Event) {
	if !n.wanted(e) {
		return
	}
	subject, body := formatEmail(e)
	go func() {
		var err error
		for attempt := 0; attempt < 2; attempt++ {
			err = n.send(subject, body)
			if err == nil {
				return
			}
			time.Sleep(2 * time.Second)
		}
		log.Printf("email alert delivery failed: %v", err)
	}()
}

func (n *emailNotifier) send(subject, body string) error {
	if n.cfg.resendKey != "" {
		return n.sendResend(subject, body)
	}
	return n.sendSMTP(subject, body)
}

func (n *emailNotifier) sendResend(subject, body string) error {
	payload := map[string]any{
		"from":    n.cfg.resendFrom,
		"to":      n.cfg.to,
		"subject": subject,
		"text":    body,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, resendEndpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.resendKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("resend: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var ok struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &ok)
	log.Printf("email: resend accepted id=%s to=%s", ok.ID, strings.Join(n.cfg.to, ","))
	return nil
}

// resendEndpoint is a variable so the transport can be exercised against a
// local server in tests instead of the real API.
var resendEndpoint = "https://api.resend.com/emails"

func (n *emailNotifier) sendSMTP(subject, body string) error {
	msg := buildEmailMessage(n.cfg.from, n.cfg.to, subject, body)
	// PLAIN is what Gmail/Mailgun/SendGrid/Resend accept; LOGIN is what most
	// Chinese providers accept. Try PLAIN, then LOGIN, rather than making the
	// operator pick an auth mode.
	errPlain := n.smtpSend(msg, "PLAIN")
	if errPlain == nil {
		return nil
	}
	if err := n.smtpSend(msg, "LOGIN"); err != nil {
		return fmt.Errorf("smtp: plain: %v; login: %v", errPlain, err)
	}
	return nil
}

func (n *emailNotifier) smtpSend(msg []byte, mode string) error {
	addr := net.JoinHostPort(n.cfg.host, strconv.Itoa(n.cfg.port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if n.cfg.tls == "implicit" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: n.cfg.host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	c, err := smtp.NewClient(conn, n.cfg.host)
	if err != nil {
		return err
	}
	defer c.Close()

	if n.cfg.tls == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: n.cfg.host}); err != nil {
			return err
		}
	}
	if n.cfg.user != "" {
		var auth smtp.Auth
		if mode == "LOGIN" {
			auth = &loginAuth{user: n.cfg.user, pass: n.cfg.pass}
		} else {
			auth = &plainAuth{user: n.cfg.user, pass: n.cfg.pass}
		}
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(fromAddress(n.cfg.from)); err != nil {
		return err
	}
	for _, to := range n.cfg.to {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// plainAuth and loginAuth are local copies because net/smtp's PlainAuth
// refuses to run on an already-TLS-wrapped connection (it only trusts its own
// STARTTLS), which is exactly the port-465 case.
type plainAuth struct{ user, pass string }

func (a *plainAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}
func (a *plainAuth) Next(fromServer []byte, more bool) ([]byte, error) { return nil, nil }

type loginAuth struct {
	user, pass string
	step       int
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	// The challenge text is informational; the order is username then password.
	defer func() { a.step++ }()
	if a.step == 0 {
		return []byte(a.user), nil
	}
	return []byte(a.pass), nil
}

func fromAddress(from string) string {
	if i := strings.LastIndex(from, "<"); i >= 0 {
		return strings.TrimSuffix(strings.TrimSpace(from[i+1:]), ">")
	}
	return strings.TrimSpace(from)
}

func buildEmailMessage(from string, to []string, subject, body string) []byte {
	var b bytes.Buffer
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")
	b.WriteString(wrapBase64(base64.StdEncoding.EncodeToString([]byte(body))))
	b.WriteString("\r\n")
	return b.Bytes()
}

func wrapBase64(s string) string {
	const width = 76
	var b strings.Builder
	for len(s) > width {
		b.WriteString(s[:width])
		b.WriteString("\r\n")
		s = s[width:]
	}
	b.WriteString(s)
	return b.String()
}

func formatEmail(e Event) (subject, body string) {
	ts := time.UnixMilli(e.At).UTC().Format("2006-01-02 15:04:05 UTC")
	tag := "INFO"
	if e.Severity == "alert" {
		tag = "ALERT"
	} else if e.Severity == "watch" {
		tag = "WATCH"
	}

	switch e.Kind {
	case kindVenueDepeg:
		subject = fmt.Sprintf("[Price Desk][%s] %s 脱锚 %.0fbps @ %s", tag, e.Symbol, e.DevBps, e.VenueName)
		body = fmt.Sprintf("币种: %s\n场所: %s (%s)\n现价: %.8g\nCEX 共识: %.8g\n偏离: %+.1f bps\n该场所基线: %+.1f bps（MAD %.1f）\n触发阈值: %.1f bps\n样本数: %d\n时间: %s\n\nPrice Desk · https://price.tsuibo.xyz\n只读告警，不触发任何交易。\n",
			e.Symbol, e.VenueName, e.MarketKind, e.Price, e.Consensus, e.DevBps, e.BaselineBps, e.MADBps, e.ThresholdBps, e.Samples, ts)
	case kindVenueNormal:
		subject = fmt.Sprintf("[Price Desk][OK] %s 回到基线 @ %s", e.Symbol, e.VenueName)
		body = fmt.Sprintf("币种: %s\n场所: %s\n偏离: %+.1f bps（基线 %+.1f）\n时间: %s\n", e.Symbol, e.VenueName, e.DevBps, e.BaselineBps, ts)
	case kindDispersion:
		subject = fmt.Sprintf("[Price Desk][%s] %s 报价离散 %.0fbps", tag, e.Symbol, e.DevBps)
		body = fmt.Sprintf("币种: %s\n离散度: %.1f bps\n基线: %.1f bps\n阈值: %.1f bps\n%s\n", e.Symbol, e.DevBps, e.BaselineBps, e.ThresholdBps, e.Note)
	case kindStablecoinPeg:
		subject = fmt.Sprintf("[Price Desk][%s] %s/USD %.6f", tag, e.Symbol, e.Price)
		body = fmt.Sprintf("资产: %s/USD\n价格: %.6f\n偏离锚定: %+.1f bps\n阈值: %.1f bps\n时间: %s\n", e.Symbol, e.Price, e.DevBps, e.ThresholdBps, ts)
	case kindVenueDown, kindVenueRecover:
		subject = fmt.Sprintf("[Price Desk][%s] %s %s", tag, e.Symbol, e.VenueName)
		body = fmt.Sprintf("币种: %s\n场所: %s\n%s\n时间: %s\n", e.Symbol, e.VenueName, e.Note, ts)
	default:
		subject = fmt.Sprintf("[Price Desk][%s] %s %s", tag, e.Symbol, e.Kind)
		body = fmt.Sprintf("%+v\n", e)
	}
	return subject, body
}

// newEmailFromEnv builds the channel only when a transport and a recipient are
// both configured. A half-configured mailbox is logged, never silently ignored.
func newEmailFromEnv() notifier {
	to := splitEmails(os.Getenv("PRICE_ALERT_EMAIL_TO"))
	if len(to) == 0 {
		return nil
	}
	cfg := emailConfig{to: to, sendAll: os.Getenv("PRICE_ALERT_EMAIL_ALL") == "1", minBps: 100}
	if v := strings.TrimSpace(os.Getenv("PRICE_ALERT_EMAIL_MIN_BPS")); v != "" {
		if fv, err := strconv.ParseFloat(v, 64); err == nil && fv >= 0 {
			cfg.minBps = fv
		}
	}
	cfg.resendKey = strings.TrimSpace(os.Getenv("PRICE_ALERT_RESEND_KEY"))
	cfg.resendFrom = strings.TrimSpace(os.Getenv("PRICE_ALERT_EMAIL_FROM"))
	if cfg.resendFrom == "" {
		cfg.resendFrom = "Price Desk <onboarding@resend.dev>"
	}
	cfg.host = strings.TrimSpace(os.Getenv("PRICE_ALERT_SMTP_HOST"))
	cfg.port = 0
	if p := strings.TrimSpace(os.Getenv("PRICE_ALERT_SMTP_PORT")); p != "" {
		cfg.port, _ = strconv.Atoi(p)
	}
	cfg.user = strings.TrimSpace(os.Getenv("PRICE_ALERT_SMTP_USER"))
	cfg.pass = strings.TrimSpace(os.Getenv("PRICE_ALERT_SMTP_PASS"))
	cfg.tls = strings.ToLower(strings.TrimSpace(os.Getenv("PRICE_ALERT_SMTP_TLS")))
	if cfg.tls == "" {
		if cfg.port == 465 {
			cfg.tls = "implicit"
		} else {
			cfg.tls = "starttls"
		}
	}
	cfg.from = strings.TrimSpace(os.Getenv("PRICE_ALERT_EMAIL_FROM"))
	if cfg.from == "" {
		cfg.from = cfg.user
	}
	if cfg.port == 0 {
		cfg.port = 587
		if cfg.tls == "implicit" {
			cfg.port = 465
		}
	}

	n := &emailNotifier{cfg: cfg, c: &http.Client{Timeout: 10 * time.Second}}
	switch {
	case cfg.resendKey != "":
		log.Printf("alerts: email via resend -> %s (gate >= %.0fbps)", strings.Join(to, ","), cfg.minBps)
		return n
	case cfg.host != "" && cfg.from != "":
		log.Printf("alerts: email via smtp %s:%d (%s) -> %s (gate >= %.0fbps)", cfg.host, cfg.port, cfg.tls, strings.Join(to, ","), cfg.minBps)
		return n
	default:
		log.Printf("alerts: PRICE_ALERT_EMAIL_TO set but no transport (need PRICE_ALERT_RESEND_KEY or PRICE_ALERT_SMTP_HOST)")
		return nil
	}
}

func splitEmails(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, err := url.Parse("mailto:" + part); err != nil || !strings.Contains(part, "@") {
			log.Printf("alerts: ignoring invalid email recipient %q", part)
			continue
		}
		out = append(out, part)
	}
	return out
}
