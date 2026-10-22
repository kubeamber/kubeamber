// Command fakehook stands in for Slack, Microsoft Teams and webhook
// receivers in the kind end-to-end test. It records every POST, and lists
// them with:
//
//	curl 'http://127.0.0.1:18081/received?path=/slack'
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type call struct {
	Path  string `json:"path"`
	Event string `json:"event"`
	// Signature is "valid", "invalid" or "none".
	Signature string          `json:"signature"`
	Body      json.RawMessage `json:"body"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "listen address")
	secret := flag.String("secret", "", "key to check X-KubeAmber-Signature with")
	flag.Parse()

	var mu sync.Mutex
	var calls []call
	mux := http.NewServeMux()
	mux.HandleFunc("GET /received", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		enc := json.NewEncoder(w)
		for _, c := range calls {
			if p := r.URL.Query().Get("path"); p == "" || p == c.Path {
				_ = enc.Encode(c)
			}
		}
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !json.Valid(body) {
			http.Error(w, "want a JSON body", http.StatusBadRequest)
			return
		}
		sig := "none"
		if got := r.Header.Get("X-KubeAmber-Signature"); got != "" {
			mac := hmac.New(sha256.New, []byte(*secret))
			mac.Write(body)
			sig = "invalid"
			if hmac.Equal([]byte(got), []byte("sha256="+hex.EncodeToString(mac.Sum(nil)))) {
				sig = "valid"
			}
		}
		mu.Lock()
		calls = append(calls, call{Path: r.URL.Path, Event: r.Header.Get("X-KubeAmber-Event"), Signature: sig, Body: body})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	log.Printf("fake notification receiver listening on http://%s", *addr)
	s := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(s.ListenAndServe())
}
