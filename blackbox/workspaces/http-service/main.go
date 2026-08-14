package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	address := os.Getenv("DAFT_LISTEN_ADDR")
	if address == "" {
		address = ":8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	if err := http.ListenAndServe(address, mux); err != nil {
		panic(err)
	}
}
