package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startFakeSMTP speaks just enough of RFC 5321 to exercise the real client:
// EHLO, AUTH PLAIN/LOGIN, MAIL, RCPT, DATA and QUIT.
func startFakeSMTP(t *testing.T) (string, int, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	data := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		send := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		send("220 fake ESMTP")

		inData := false
		login := 0
		var body strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					data <- body.String()
					send("250 OK queued")
					continue
				}
				body.WriteString(line + "\n")
				continue
			}
			up := strings.ToUpper(line)
			switch {
			case login == 1:
				login = 2
				send("334 UGFzc3dvcmQ6")
			case login == 2:
				login = 0
				send("235 authenticated")
			case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
				send("250-fake")
				send("250-AUTH PLAIN LOGIN")
				send("250 OK")
			case strings.HasPrefix(up, "AUTH PLAIN"):
				send("235 authenticated")
			case strings.HasPrefix(up, "AUTH LOGIN"):
				login = 1
				send("334 VXNlcm5hbWU6")
			case strings.HasPrefix(up, "MAIL FROM"), strings.HasPrefix(up, "RCPT TO"):
				send("250 OK")
			case up == "DATA":
				inData = true
				send("354 end with <CRLF>.<CRLF>")
			case up == "QUIT":
				send("221 bye")
				return
			default:
				send("250 OK")
			}
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port, data
}

func TestEmailSMTPSendsMessage(t *testing.T) {
	host, port, data := startFakeSMTP(t)
	n := &emailNotifier{
		cfg: emailConfig{
			to: []string{"ops@example.com"}, from: "Price Desk <bot@example.com>",
			host: host, port: port, user: "bot@example.com", pass: "secret", tls: "none",
		},
		c: &http.Client{Timeout: 5 * time.Second},
	}
	subject, body := formatEmail(Event{
		At: time.Now().UnixMilli(), Kind: kindVenueDepeg, Symbol: "BTC",
		Venue: "binance", VenueName: "Binance", MarketKind: "cex",
		Price: 68000, Consensus: 69340, DevBps: -193.2, BaselineBps: 1.8,
		MADBps: 2.1, ThresholdBps: 45, Severity: "alert", Samples: 42,
	})
	if err := n.send(subject, body); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case msg := <-data:
		if !strings.Contains(msg, "Subject: ") {
			t.Fatalf("no subject header: %q", msg[:120])
		}
		// The body is base64; decode the payload after the blank line.
		parts := strings.SplitN(msg, "\n\n", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed message: %q", msg)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\n", ""))
		if err != nil {
			t.Fatalf("body not base64: %v", err)
		}
		if !strings.Contains(string(decoded), "偏离: -193.2 bps") {
			t.Fatalf("body missing numbers: %q", decoded)
		}
		if !strings.Contains(string(decoded), "只读告警") {
			t.Fatalf("body missing disclaimer: %q", decoded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake SMTP server received no message")
	}
}

func TestEmailSMTPLoginAuth(t *testing.T) {
	host, port, data := startFakeSMTP(t)
	n := &emailNotifier{
		cfg: emailConfig{
			to: []string{"ops@example.com"}, from: "bot@example.com",
			host: host, port: port, user: "bot@example.com", pass: "secret", tls: "none",
		},
		c: &http.Client{Timeout: 5 * time.Second},
	}
	msg := buildEmailMessage("bot@example.com", []string{"ops@example.com"}, "s", "b")
	if err := n.smtpSend(msg, "LOGIN"); err != nil {
		t.Fatalf("login auth: %v", err)
	}
	select {
	case got := <-data:
		if !strings.Contains(got, "Subject: ") {
			t.Fatalf("message not delivered: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func TestEmailResendTransport(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	old := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = old }()

	n := &emailNotifier{
		cfg: emailConfig{to: []string{"ops@example.com"}, resendKey: "test-key", resendFrom: "Price Desk <onboarding@resend.dev>"},
		c: &http.Client{Timeout: 5 * time.Second},
	}
	if err := n.send("subject", "body"); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if got["subject"] != "subject" || got["text"] != "body" {
		t.Fatalf("payload = %+v", got)
	}
	if to, ok := got["to"].([]any); !ok || len(to) != 1 || to[0] != "ops@example.com" {
		t.Fatalf("recipients = %+v", got["to"])
	}
}

func TestEmailOnlyMailsActionableEvents(t *testing.T) {
	n := &emailNotifier{cfg: emailConfig{minBps: 100}}
	cases := []struct {
		e    Event
		want bool
	}{
		{Event{Kind: kindVenueDepeg, Symbol: "BTC", Severity: "alert", DevBps: 250}, true},
		{Event{Kind: kindVenueDepeg, Symbol: "BTC", Severity: "watch", DevBps: 250}, false},
		{Event{Kind: kindVenueDepeg, Symbol: "BTC", Severity: "alert", DevBps: 80}, false},
		{Event{Kind: kindDispersion, Symbol: "BTC", Severity: "watch", DevBps: 400}, false},
		{Event{Kind: kindVenueDown, Symbol: "BTC", Venue: "binance", Severity: "watch"}, true},
		// A recovery is only sent if the outage itself was mailed.
		{Event{Kind: kindVenueRecover, Symbol: "BTC", Venue: "binance", Severity: "info"}, true},
		{Event{Kind: kindVenueRecover, Symbol: "BTC", Venue: "binance", Severity: "info"}, false},
	}
	for i, c := range cases {
		if got := n.wanted(c.e); got != c.want {
			t.Fatalf("case %d (%s/%s): wanted=%v got=%v", i, c.e.Kind, c.e.Severity, c.want, got)
		}
	}
	n.cfg.sendAll = true
	if !n.wanted(Event{Kind: kindDispersion, Severity: "watch"}) {
		t.Fatal("sendAll should forward watch events")
	}
}

func TestEmailResolutionOnlyFollowsAMailedAlert(t *testing.T) {
	n := &emailNotifier{cfg: emailConfig{minBps: 100}}

	// Watch-level wobble: never mailed, so its resolution must stay silent.
	if n.wanted(Event{Kind: kindVenueDepeg, Symbol: "WLD", Venue: "cryptocom", Severity: "watch", DevBps: 90}) {
		t.Fatal("watch-level depeg should not be mailed")
	}
	if n.wanted(Event{Kind: kindVenueNormal, Symbol: "WLD", Venue: "cryptocom", Severity: "info", DevBps: 2}) {
		t.Fatal("resolution of an unmailed watch alert was mailed")
	}

	// Alert-level: opening and its resolution both go out, exactly once.
	if !n.wanted(Event{Kind: kindVenueDepeg, Symbol: "XLM", Venue: "htx", Severity: "alert", DevBps: 420}) {
		t.Fatal("alert-level depeg should be mailed")
	}
	if !n.wanted(Event{Kind: kindVenueNormal, Symbol: "XLM", Venue: "htx", Severity: "info", DevBps: 5}) {
		t.Fatal("resolution of a mailed alert should be mailed")
	}
	if n.wanted(Event{Kind: kindVenueNormal, Symbol: "XLM", Venue: "htx", Severity: "info", DevBps: 5}) {
		t.Fatal("resolution was mailed twice")
	}

	// Peg keeps its own absolute test, and its recovery follows the same rule.
	if !n.wanted(Event{Kind: kindStablecoinPeg, Symbol: "USDT", Severity: "alert", DevBps: 80}) {
		t.Fatal("peg break should be mailed")
	}
	if !n.wanted(Event{Kind: kindStablecoinPeg, Symbol: "USDT", Severity: "info", DevBps: 2}) {
		t.Fatal("peg recovery should be mailed")
	}
	if n.wanted(Event{Kind: kindStablecoinPeg, Symbol: "USDT", Severity: "info", DevBps: 2}) {
		t.Fatal("peg recovery mailed twice")
	}
}

func TestMultiNotifierFanout(t *testing.T) {
	a := &recordingNotifier{name: "a"}
	b := &recordingNotifier{name: "b"}
	m := &multiNotifier{list: []notifier{logNotifier{}, a, b}}
	m.Notify(Event{Kind: kindVenueDepeg, Symbol: "BTC", Severity: "watch"})
	if len(a.events) != 1 || len(b.events) != 1 {
		t.Fatalf("fanout = %d/%d", len(a.events), len(b.events))
	}
	if m.Name() != "a+b" {
		t.Fatalf("name = %q", m.Name())
	}
}

type recordingNotifier struct {
	name   string
	events []Event
}

func (r *recordingNotifier) Name() string { return r.name }
func (r *recordingNotifier) Notify(e Event) {
	r.events = append(r.events, e)
}