package guards

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeamber/kubeamber/pkg/policy"
)

func TestPrometheusGuard(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query().Get("query")
		switch q {
		case "burning":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"20"]}]}}`)
		case "healthy":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		case "scalar":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"scalar","result":[1,"1"]}}`)
		default:
			fmt.Fprint(w, `{"status":"error","error":"parse error"}`)
		}
	}))
	defer srv.Close()

	cfg := &policy.PrometheusGuard{URL: srv.URL, Timeout: metav1.Duration{Duration: time.Second}, OnError: policy.Hold,
		Checks: []policy.PrometheusCheck{
			{Name: "burn", Query: "burning", Action: policy.Hold, Reason: "SLO burning", Namespaces: []string{"prod"}},
			{Name: "ok", Query: "healthy", Action: policy.Deny},
			{Name: "broken", Query: "nonsense{", Action: policy.Hold, Namespaces: []string{"broken"}},
			{Name: "scalar", Query: "scalar", Action: policy.Hold, Namespaces: []string{"scalar"}},
		}}
	g := NewPrometheus()
	ctx := context.Background()

	got := g.Evaluate(ctx, cfg, "prod")
	if len(got) != 1 || got[0].Name != "burn" || got[0].Verdict != policy.Hold || !strings.Contains(got[0].Reason, "SLO burning") {
		t.Errorf("prod: %+v", got)
	}
	if got := g.Evaluate(ctx, cfg, "shop"); len(got) != 0 {
		t.Errorf("shop should not fire: %+v", got)
	}
	if got := g.Evaluate(ctx, cfg, "broken"); len(got) != 1 || !strings.Contains(got[0].Reason, "could not be evaluated") {
		t.Errorf("errors should hold (onError): %+v", got)
	}
	if got := g.Evaluate(ctx, cfg, "scalar"); len(got) != 1 || !strings.Contains(got[0].Reason, "vector") {
		t.Errorf("scalar results should be an error: %+v", got)
	}

	before := calls.Load()
	g.Evaluate(ctx, cfg, "prod")
	if calls.Load() != before {
		t.Error("cached results were re-queried")
	}

	cfg.OnError = policy.Allow
	g = NewPrometheus()
	if got := g.Evaluate(ctx, cfg, "broken"); len(got) != 0 {
		t.Errorf("onError allow should not fire: %+v", got)
	}
}
