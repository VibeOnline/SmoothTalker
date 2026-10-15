package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"
)

const (
	target        = "http://127.0.0.1:11434"
	listenAddr    = ":1337"
	routerModel   = "nimble:9b"
	fallbackModel = "gemma4:e2b-mlx"
	maxBodyBytes  = 10 << 20 // 10 MiB
)

// Used only for the proxy's own side calls (tags + router), not for proxied traffic.
var client = &http.Client{Timeout: 30 * time.Second}

type ollamaModelList struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

type routerAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type routerResponse struct {
	Error   string                  `json:"error"`
	Answers map[string]routerAnswer `json:"answers"`
}

func main() {
	remote, err := url.Parse(target)
	if err != nil {
		log.Fatal("Invalid target URL: ", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(remote)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
			return
		}

		if r.Method == http.MethodPost && (r.URL.Path == "/api/chat" || r.URL.Path == "/api/generate" || r.URL.Path == "/v1/chat/completions") {
			if !rewriteRequest(w, r) {
				return // error already written
			}
		}

		log.Printf("Proxying request: %s %s -> %s", r.Method, r.URL.Path, target)
		proxy.ServeHTTP(w, r)
	})

	log.Printf("Server starting on %s, proxying requests to %s", listenAddr, target)
	if err := http.ListenAndServe(listenAddr, handler); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

// rewriteRequest reads the body, resolves "recommended", injects options,
// and replaces r.Body. Returns false if it already wrote an error response.
func rewriteRequest(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	r.Body.Close()
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Error reading request body", http.StatusBadRequest)
		}
		return false
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return false
	}

	if model, _ := req["model"].(string); model == "recommended" {
		req["model"] = chooseModel(r.Context(), req, r.URL.Path)
	}

	// NOTE: Stock Ollama uses a top-level "think" field rather than this option.
	// Keep this only if your Ollama build actually reads options.thinking_level.
	options, ok := req["options"].(map[string]any)
	if !ok {
		options = make(map[string]any)
		req["options"] = options
	}
	options["thinking_level"] = 5

	newBody, err := json.Marshal(req)
	if err != nil {
		http.Error(w, "Error encoding request body", http.StatusInternalServerError)
		return false
	}

	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	return true
}

// chooseModel always returns a usable model name, falling back on any failure.
func chooseModel(ctx context.Context, req map[string]any, path string) string {
	log.Printf("Model is 'recommended'. Determining best model via /v1/systemone...")

	models, err := fetchModels(ctx)
	if err != nil {
		log.Printf("Error fetching models: %v. Falling back to %s.", err, fallbackModel)
		return fallbackModel
	}

	// Exclude the router itself from the candidates.
	candidates := make(map[string]string)
	for _, name := range models {
		if name != routerModel {
			candidates[name] = fmt.Sprintf("Use the %s model", name)
		}
	}
	if len(candidates) == 0 {
		log.Printf("No candidate models available. Falling back to %s.", fallbackModel)
		return fallbackModel
	}

	choice, err := askRouter(ctx, extractInput(req, path), candidates)
	if err != nil {
		log.Printf("Router failed: %v. Falling back to %s.", err, fallbackModel)
		return fallbackModel
	}
	if _, valid := candidates[choice]; !valid {
		log.Printf("Router chose unknown model %q. Falling back to %s.", choice, fallbackModel)
		return fallbackModel
	}

	log.Printf("Router (%s) chose model: %s", routerModel, choice)
	return choice
}

func fetchModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/api/tags returned status %d", resp.StatusCode)
	}

	var list ollamaModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding /api/tags: %w", err)
	}

	names := make([]string, 0, len(list.Models))
	for _, m := range list.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func askRouter(ctx context.Context, input string, candidates map[string]string) (string, error) {
	payload := map[string]any{
		"model": routerModel,
		"state": input,
		"questions": map[string]any{
			"best_model": map[string]any{
				"type":         "choice",
				"instructions": "Which model is best for this request? Provide only the model name.",
				"criteria":     candidates,
			},
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/v1/systemone", bytes.NewReader(payloadJSON))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading router response: %w", err)
	}

	log.Printf("Router raw response (status %d): %s", resp.StatusCode, body)

	var result routerResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decoding router response (status %d): %w", resp.StatusCode, err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("router error: %s", result.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("router returned status %d", resp.StatusCode)
	}

	ans, ok := result.Answers["best_model"]
	if !ok || ans.Choice == "" {
		return "", fmt.Errorf("router returned no model: %s", body)
	}
	log.Printf("Router choice: %s (confidence %.2f, probs %v)", ans.Choice, ans.Confidence, ans.Probabilities)
	return ans.Choice, nil
}

// extractInput pulls the latest user text without producing "<nil>".
func extractInput(req map[string]any, path string) string {
	var s string
	if path == "/api/chat" {
		if msgs, ok := req["messages"].([]any); ok && len(msgs) > 0 {
			if m, ok := msgs[len(msgs)-1].(map[string]any); ok {
				s, _ = m["content"].(string)
			}
		}
	} else {
		s, _ = req["prompt"].(string)
	}
	if s == "" {
		return "Empty request"
	}
	return s
}
