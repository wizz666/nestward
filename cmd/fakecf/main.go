// Command fakecf runs the in-memory fake Cloudflare API for trying the
// publish flow locally. For development only; it is not part of the
// Nestward program.
//
//	go run ./cmd/fakecf -addr 127.0.0.1:8490 -token dev-token
//	NESTWARD_CLOUDFLARE_API=http://127.0.0.1:8490 nestward serve
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/wizz666/nestward/internal/cloudflare/cftest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8490", "address to listen on")
	token := flag.String("token", "dev-token", "token the fake accepts")
	flag.Parse()
	log.Printf("fake Cloudflare API on http://%s (token %q)", *addr, *token)
	log.Fatal(http.ListenAndServe(*addr, cftest.New(*token)))
}
