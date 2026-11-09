package frontend

import (
	"encoding/json"
	"net/http"
)

// maxBodyBytes is the largest JSON body the frontend reads itself (the passkey ceremony).
const maxBodyBytes = 4 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
