package api

import (
	"net/http"
)

func RootHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"name": "Salkaro API", "version": "1.0.1", "status": "active"}`))
}
