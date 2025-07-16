package main

import (
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

	// Register endpoints
	http.HandleFunc("/", api.RootHandler)
	http.HandleFunc("/v1/upload", api.UploadHandler)

	log.Println("📡 DeviceData API listening on :8080")
	log.Println("✅ /v1/upload endpoint is now available for local development")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
