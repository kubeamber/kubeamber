package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type capture struct {
	body    []byte
	headers http.Header
}

func receiver(t *testing.T, status int) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.body, _ = io.ReadAll(r.Body)
		c.headers = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte("no_service"))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sample() Message {
	cr := held("cr-1", t0)
	cr.Spec.Intent = "ping <!channel> & <https://evil.example|click>"
	return NewMessage(cr, Held, "prod-eu-1")
}

func TestSlackPayload(t *testing.T) {
	srv, got := receiver(t, http.StatusOK)
	ch := NewSlack("platform", writeFile(t, srv.URL+"/services/T/B/x"), nil, "kubeamber/test")
	if err := ch.Send(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Text   string `json:"text"`
		Blocks []struct {
			Type string `json:"type"`
			Text struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 3 || p.Blocks[0].Type != "header" || !strings.HasPrefix(p.Blocks[0].Text.Text, "[prod-eu-1] Approval needed") {
		t.Fatalf("blocks: %+v", p.Blocks)
	}
	body := p.Blocks[1].Text.Text
	if !strings.Contains(body, "kubeamber approve cr-1") {
		t.Errorf("no approve command in %q", body)
	}
	if strings.Contains(body, "<!channel>") || strings.Contains(body, "<https://") || !strings.Contains(body, "&lt;!channel&gt; &amp;") {
		t.Errorf("agent text was not escaped: %q", body)
	}
	if got.headers.Get("X-KubeAmber-Event") != "held" || got.headers.Get("User-Agent") != "kubeamber/test" {
		t.Errorf("headers: %v", got.headers)
	}
}

func TestTeamsPayload(t *testing.T) {
	srv, got := receiver(t, http.StatusAccepted)
	ch := NewTeams("oncall", writeFile(t, srv.URL), nil, "kubeamber/test")
	if err := ch.Send(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string `json:"contentType"`
			Content     struct {
				Type string `json:"type"`
				Body []struct {
					Text string `json:"text"`
				} `json:"body"`
			} `json:"content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "message" || len(p.Attachments) != 1 || p.Attachments[0].ContentType != "application/vnd.microsoft.card.adaptive" ||
		p.Attachments[0].Content.Type != "AdaptiveCard" {
		t.Fatalf("payload: %s", got.body)
	}
	if !strings.Contains(string(got.body), "kubeamber approve cr-1") {
		t.Errorf("no approve command: %s", got.body)
	}
}

func TestWebhookIsSignedJSON(t *testing.T) {
	srv, got := receiver(t, http.StatusNoContent)
	ch := NewWebhook("audit", writeFile(t, srv.URL), writeFile(t, "s3cret"), nil, "kubeamber/test")
	if err := ch.Send(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	var m Message
	if err := json.Unmarshal(got.body, &m); err != nil || m.Event != Held || m.ChangeRequest != "cr-1" || m.Risk != 50 {
		t.Fatalf("message %+v, %v", m, err)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(got.body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); got.headers.Get(SignatureHeader) != want {
		t.Errorf("signature %q, want %q", got.headers.Get(SignatureHeader), want)
	}
}

func TestChannelErrorsDoNotLeakTheURL(t *testing.T) {
	srv, _ := receiver(t, http.StatusNotFound)
	secretURL := srv.URL + "/services/T000/B000/SECRETTOKEN"
	err := NewSlack("platform", writeFile(t, secretURL), nil, "kubeamber/test").Send(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("error %v", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	unreachable := closed.URL + "/SECRETTOKEN"
	closed.Close()
	err = NewWebhook("audit", writeFile(t, unreachable), "", nil, "kubeamber/test").Send(context.Background(), sample())
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("error %v", err)
	}
}

func TestURLIsReadOnEverySend(t *testing.T) {
	first, a := receiver(t, http.StatusOK)
	second, b := receiver(t, http.StatusOK)
	path := writeFile(t, first.URL)
	ch := NewWebhook("audit", path, "", nil, "kubeamber/test")
	_ = ch.Send(context.Background(), sample())
	if err := os.WriteFile(path, []byte(second.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = ch.Send(context.Background(), sample())
	if a.body == nil || b.body == nil {
		t.Fatal("a rotated URL was not picked up")
	}
}
