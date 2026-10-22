package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Channel delivers messages to one place.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// SignatureHeader carries the HMAC-SHA256 of a webhook body, as
// "sha256=<hex>", when the channel has a secret.
const SignatureHeader = "X-KubeAmber-Signature"

// poster sends JSON to a URL read from a file on every send, so a rotated
// Secret takes effect without a restart.
type poster struct {
	name       string
	urlFile    string
	secretFile string
	client     *http.Client
	userAgent  string
}

func (p *poster) Name() string { return p.name }

func (p *poster) post(ctx context.Context, event Event, payload any) error {
	endpoint, err := readTrimmed(p.urlFile)
	if err != nil {
		return fmt.Errorf("channel %s: reading the URL: %w", p.name, err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// The URL itself is a secret, so it is left out of the error.
		return fmt.Errorf("channel %s: invalid URL", p.name)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("X-KubeAmber-Event", string(event))
	if p.secretFile != "" {
		secret, err := readTrimmed(p.secretFile)
		if err != nil {
			return fmt.Errorf("channel %s: reading the signing secret: %w", p.name, err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set(SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("channel %s: %w", p.name, redactURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("channel %s: HTTP %d: %s", p.name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// Slack posts to an incoming webhook, with Block Kit formatting.
type Slack struct{ poster }

// Teams posts an Adaptive Card to a Microsoft Teams Workflows webhook.
type Teams struct{ poster }

// Webhook posts the Message as JSON, signed when a secret is set.
type Webhook struct{ poster }

// NewSlack, NewTeams and NewWebhook build channels. A nil client gets a
// 10-second timeout.
func NewSlack(name, urlFile string, client *http.Client, userAgent string) *Slack {
	return &Slack{newPoster(name, urlFile, "", client, userAgent)}
}

func NewTeams(name, urlFile string, client *http.Client, userAgent string) *Teams {
	return &Teams{newPoster(name, urlFile, "", client, userAgent)}
}

func NewWebhook(name, urlFile, secretFile string, client *http.Client, userAgent string) *Webhook {
	return &Webhook{newPoster(name, urlFile, secretFile, client, userAgent)}
}

func newPoster(name, urlFile, secretFile string, client *http.Client, userAgent string) poster {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return poster{name: name, urlFile: urlFile, secretFile: secretFile, client: client, userAgent: userAgent}
}

func (s *Slack) Send(ctx context.Context, m Message) error {
	title := m.Title
	if m.Cluster != "" {
		title = "[" + m.Cluster + "] " + title
	}
	lines := m.Lines()
	for i, l := range lines {
		lines[i] = slackEscape(l)
	}
	payload := map[string]any{
		"text": slackEscape(title),
		"blocks": []map[string]any{
			{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncateRunes(title, 150)}},
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncateRunes(strings.Join(lines, "\n"), 3000)}},
			{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": "KubeAmber · " + slackEscape(m.ChangeRequest)}}},
		},
	}
	return s.post(ctx, m.Event, payload)
}

func (t *Teams) Send(ctx context.Context, m Message) error {
	title := m.Title
	if m.Cluster != "" {
		title = "[" + m.Cluster + "] " + title
	}
	body := []map[string]any{
		{"type": "TextBlock", "text": title, "weight": "Bolder", "size": "Medium", "wrap": true},
	}
	for _, l := range m.Lines() {
		body = append(body, map[string]any{"type": "TextBlock", "text": l, "wrap": true, "spacing": "Small"})
	}
	payload := map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard",
				"version": "1.4",
				"body":    body,
			},
		}},
	}
	return t.post(ctx, m.Event, payload)
}

func (w *Webhook) Send(ctx context.Context, m Message) error {
	return w.post(ctx, m.Event, m)
}

// slackEscape escapes the three characters Slack's mrkdwn treats as markup, so
// text an agent wrote cannot mention @channel or forge links.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// redactURL drops the URL from an HTTP client error: webhook URLs carry
// their credentials in the path.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
