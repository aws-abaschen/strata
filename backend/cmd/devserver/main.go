// Command devserver runs the Strata API locally with an in-memory store and no
// AWS. The bearer token is taken as the user's identity, so never expose it.
//
//	go run ./cmd/devserver            # http://localhost:8788
//	web/public/config.json: {"apiUrl":"http://localhost:8788","devUser":"you@example.com"}
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"github.com/aws-abaschen/strata/backend/internal/api"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

func main() {
	addr := flag.String("addr", "localhost:8788", "listen address")
	flag.Parse()
	mem := store.NewMemory()
	mem.BaseURL = "http://" + *addr
	h := (&api.API{Objects: mem, Meta: mem, WebURL: "http://localhost:5173"}).Handler()

	mux := http.NewServeMux()
	mux.Handle("/_objects/", mem.UploadHandler())
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer ")
		h.ServeHTTP(w, r.WithContext(api.WithIdentity(r.Context(), id)))
	}))
	cors := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-headers", "authorization, content-type")
		w.Header().Set("access-control-allow-methods", "GET, POST, PUT")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
	log.Printf("strata dev API on http://%s (in-memory, insecure)", *addr)
	log.Fatal(http.ListenAndServe(*addr, cors))
}
