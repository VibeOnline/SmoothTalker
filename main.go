package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

// OllamaRequest represents the expected structure of the /api/generate request
type OllamaRequest struct {
	Model    string                 `json:"model"`
	Prompt   string                 `json:"prompt"`
	Stream   bool                   `json:"stream"`
	Options map[string]interface{} `json:"options,omitempty"`
	Raw      map[string]interface{} `json:"-"` // Catch-all for other fields
}

func main() {
	target := "http://127.0.0.1:11434"
	remote, err := url.Parse(target)
	if err != nil {
		log.Fatal("Invalid target URL:", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(remote)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
			return
		}

		// Intercept /api/generate to add a thinking level/option
		if r.URL.Path == "/api/generate" && r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Error reading request body", http.StatusBadRequest)
				return
			}
			defer r.Body.Close()

			var req map[string]interface{}
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "Invalid JSON", http.StatusBadRequest)
				return
			}

			// Initialize options if it doesn't exist
			options, ok := req["options"].(map[string]interface{})
			if !ok {
				options = make(map[string]interface{})
				req["options"] = options
			}

			// Add the thinking level (assuming "num_predict" or a custom parameter 
			// as Ollama doesn't have a native "thinking_level" field, 
			// but often reasoning models use specific options).
			// Here we'll add a dummy 'thinking_level' or similar parameter.
			options["thinking_level"] = 5 
			log.Printf("Injected thinking_level=5 into request for model: %v", req["model"])

			newBody, err := json.Marshal(req)
			if err != nil {
				http.Error(w, "Error encoding request body", http.StatusInternalServerError)
				return
			}

			// Replace the request body for the proxy
			r.Body = io.NopCloser(bytes.NewBuffer(newBody))
			r.ContentLength = int64(len(newBody))
			r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
		}

		log.Printf("Proxying request: %s %s -> %s", r.Method, r.URL.Path, target)
		proxy.ServeHTTP(w, r)
	})

	port := ":8080"
	log.Printf("Server starting on %s, proxying unknown requests to %s", port, target)
	if err := http.ListenAndServe(port, handler); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
