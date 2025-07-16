package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"
	"golang.org/x/time/rate"
	"google.golang.org/api/option"
)

type IReadingType any

type SensorReading struct {
	SensorID  *string      `json:"sensorId"`
	Timestamp *int64       `json:"timestamp"`
	Value     IReadingType `json:"value"`
	Status    *string      `json:"status"`
}

var (
	// Rate‐limiters per (apiKey, sensorID)
	limiters = make(map[string]*rate.Limiter)
	mu       sync.Mutex

	// Firestore client
	firestoreClient *firestore.Client
	fsCtx           = context.Background()

	// InfluxDB v3 client
	influxClient *influxdb3.Client

	// Retention‐to‐bucket mapping
	retentionBuckets = map[string]string{
		"0007": "retention_7d",
		"0030": "retention_30d",
		"0090": "retention_90d",
		"0180": "retention_180d",
		"0365": "retention_365d",
	}

	// Initialization flag to prevent multiple initializations
	initialized = false
	initMu      sync.Mutex
)

func initClients() error {
	initMu.Lock()
	defer initMu.Unlock()

	if initialized {
		return nil
	}

	// Firestore setup with environment-based credentials
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		return errors.New("PROJECT_ID environment variable not set")
	}

	var client *firestore.Client
	var err error

	// Check if Firebase credentials are provided as environment variable (Vercel)
	if firebaseCredentials := os.Getenv("FIREBASE_CREDENTIALS"); firebaseCredentials != "" {
		client, err = firestore.NewClient(fsCtx, projectID, option.WithCredentialsJSON([]byte(firebaseCredentials)))
	} else {
		// Fallback to file-based credentials for local development
		client, err = firestore.NewClient(fsCtx, projectID, option.WithCredentialsFile("config/firebase-credentials.json"))
	}

	if err != nil {
		return err
	}
	firestoreClient = client

	// InfluxDB v3 setup
	influxURL := os.Getenv("INFLUXDB_URL")
	influxToken := os.Getenv("INFLUXDB_TOKEN")
	if influxURL == "" || influxToken == "" {
		return errors.New("INFLUXDB_URL or INFLUXDB_TOKEN environment variable not set")
	}

	influxClient, err = influxdb3.New(influxdb3.ClientConfig{
		Host:  influxURL,
		Token: influxToken,
	})
	if err != nil {
		return err
	}

	initialized = true
	return nil
}

// Handler is the main Vercel serverless function handler
func UploadHandler(w http.ResponseWriter, r *http.Request) {
	// Initialize clients on first request (cold start optimization)
	if err := initClients(); err != nil {
		log.Printf("Failed to initialize clients: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	handleUpload(w, r)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	// Step 1: Check Method
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Step 2: Check auth header
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	apiKey := strings.TrimPrefix(auth, "Bearer ")

	// Step 3: Extract query params
	orgID := r.URL.Query().Get("org_id")
	sensorID := r.URL.Query().Get("sensor_id")
	if orgID == "" || sensorID == "" {
		http.Error(w, "Missing org_id or sensor_id query parameter", http.StatusBadRequest)
		return
	}

	// Step 4: Validate API key, permissions and sensor id
	if (!validateAPIKey(orgID, apiKey) || !validateSensorID(orgID, sensorID)){
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Step 5: Rate‐limit per (apiKey, sensorID)
	limiter := getRateLimiter(apiKey, sensorID)
	if !limiter.Allow() {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Step 6: Decode payload
	var reading SensorReading
	if err := json.NewDecoder(r.Body).Decode(&reading); err != nil {
		http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if reading.SensorID == nil || *reading.SensorID == "" {
		reading.SensorID = &sensorID
	}
	if reading.Timestamp == nil {
		http.Error(w, "Missing timestamp", http.StatusBadRequest)
		return
	}
	raw := *reading.Timestamp

	var ts time.Time
	if raw < 1_000_000_000_000 {
		// likely seconds
		ts = time.Unix(raw, 0).UTC()
	} else {
		// milliseconds
		ts = time.UnixMilli(raw).UTC()
	}
	if err := validateReading(reading); err != nil {
		http.Error(w, "Bad Reading: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Step 7: Extract retention code and select bucket
	retentionCode := apiKey[len(apiKey)-6 : len(apiKey)-2]
	bucket := retentionBuckets[retentionCode]
	if bucket == "" {
		http.Error(w, "Unknown retention level", http.StatusBadRequest)
		return
	}

	// Step 8: Check count limit
	if !validateCountLimit(orgID, retentionCode, bucket, w) {
		return
	}

	// Step 9: Write to InfluxDB
	tags := map[string]string{"org": orgID, "sensor": *reading.SensorID}
	fields := map[string]interface{}{"value": reading.Value}
	if reading.Status != nil {
		fields["status"] = *reading.Status
	}
	point := influxdb3.NewPoint("sensor_reading", tags, fields, ts)

	// Step 10: Convert point to line protocol format
	lineProtocolBytes, err := point.MarshalBinary(0) // 0 for nanosecond precision
	if err != nil {
		log.Printf("Failed to marshal point: %v", err)
		http.Error(w, "Failed to process data", http.StatusInternalServerError)
		return
	}

	// Step 11: Write to InfluxDB with proper bucket using WriteOptions
	writeOptions := []influxdb3.WriteOption{
		influxdb3.WithDatabase(bucket),
	}
	if err := influxClient.Write(context.Background(), lineProtocolBytes, writeOptions...); err != nil {
		log.Printf("Influx write error: %v", err)
		http.Error(w, "Failed to write data", http.StatusInternalServerError)
		return
	}

	// Step 12: Respond
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"accepted"}`))
}

func validateCountLimit(orgID, retentionCode, bucket string, w http.ResponseWriter) bool {
	quotaMap := map[string]int64{
		"0007": 10_000,
		"0030": 100_000,
		"0090": 1_000_000,
		"0180": 10_000_000,
		"0365": 1_000_000_000,
	}
	maxPoints, ok := quotaMap[retentionCode]
	if !ok {
		http.Error(w, "Unknown retention tier", http.StatusBadRequest)
		return false
	}

	sql := fmt.Sprintf(`
		SELECT COUNT(*) AS count FROM sensor_reading WHERE "org" = '%s'
	`, orgID)

	iterator, err := influxClient.Query(context.Background(), sql, influxdb3.WithDatabase(bucket))
	if err != nil {
		log.Printf("(validateCountLimit) Influx query error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return false
	}

	var currentCount int64
	for !iterator.Done() {
		if !iterator.Next() {
			break
		}
		v := iterator.Value()
		if num, ok := v["count"].(int64); ok { // values come as map[string]interface{}
			currentCount = num
		}
	}
	if iterator.Err() != nil {
		log.Printf("(validateCountLimit) Error reading count: %v", iterator.Err())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return false
	}

	if currentCount >= maxPoints {
		http.Error(w, "Quota exceeded", http.StatusPaymentRequired)
		return false
	}
	return true
}

func validateAPIKey(orgID, apiKey string) bool {
	// Step 1: Check API key permissions
	if !isUploadAllowed(apiKey) {
		return false
	}

	// Step 2: Check to see if the api key exists
	docRef := firestoreClient.
		Collection("tokens").
		Doc(orgID).
		Collection("apiKeys").
		Doc(apiKey)

	// Use a short timeout to avoid hanging
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := docRef.Get(ctx)
	if err != nil {
		// Not found or some other error
		return false
	}
	return true
}

func validateSensorID(orgID, sensorId string) bool {
	// Step 1: Check to see if the api key exists
	docRef := firestoreClient.
		Collection("devices").
		Doc(orgID).
		Collection("sensors").
		Doc(sensorId)

	// Use a short timeout to avoid hanging
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := docRef.Get(ctx)
	if err != nil {
		// Not found or some other error
		return false
	}
	return true
}

func validateReading(r SensorReading) error {
	if r.Value == nil {
		return errors.New("value cannot be null")
	}
	switch r.Value.(type) {
	case float64, string, bool:
		return nil
	default:
		return errors.New("value must be number, string, or boolean")
	}
}

func getRateLimiter(apiKey string, sensorID string) *rate.Limiter {
	key := apiKey + ":" + sensorID

	mu.Lock()
	defer mu.Unlock()

	limiter, exists := limiters[key]
	if !exists {
		limiter = rate.NewLimiter(2, 2) // 2 events per second, burst size 2
		limiters[key] = limiter
	}

	return limiter
}

func isUploadAllowed(apiKey string) bool {
	if len(apiKey) < 2 {
		return false
	}

	accessLevel := apiKey[len(apiKey)-2:]
	switch accessLevel {
	case "02", "03", "04":
		return true
	default:
		return false
	}
}
