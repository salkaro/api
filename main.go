package main

import (
	"fmt"
	"influxdb_go_client/api"
	"log"
	"net/http"

	"github.com/joho/godotenv"
)

func main() {
	// Load environment variables from .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name": "Salkaro API", "version": "1.0.1"}`)
	})

	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"healthy"}`))
	})

	// Register the upload endpoint
	http.HandleFunc("/v1/upload", api.Handler)

	log.Println("📡 DeviceData API listening on :8080")
	log.Println("✅ /v1/upload endpoint is now available for local development")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
