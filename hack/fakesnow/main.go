// Command fakesnow runs a fake ServiceNow instance for demos and the kind
// end-to-end test. Approve a change the way a CAB would with:
//
//	curl -X POST 'http://127.0.0.1:18080/fake/changes/CHG0030001/approve?by=jane.cab'
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/kubeamber/kubeamber/internal/changes/servicenow/fake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	user := flag.String("user", "kubeamber.integration", "basic-auth user name")
	pass := flag.String("password", "demo-only", "basic-auth password")
	clientID := flag.String("client-id", "kubeamber", "OAuth client id")
	clientSecret := flag.String("client-secret", "demo-only", "OAuth client secret")
	flag.Parse()

	srv := fake.New(*user, *pass, *clientID, *clientSecret)
	log.Printf("fake ServiceNow listening on http://%s", *addr)
	s := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(s.ListenAndServe())
}
