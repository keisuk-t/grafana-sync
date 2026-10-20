// The probe receives decrypted headers from disposable Grafana test containers.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func main() {
	server := &http.Server{
		Addr:              ":8080",
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{"secret": r.Header.Get("X-Sync-Test")}); err != nil {
				log.Print(err)
			}
		}),
	}
	log.Fatal(server.ListenAndServe())
}
