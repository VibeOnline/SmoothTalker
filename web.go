package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
)

//go:embed web
var webFS embed.FS

// webAssets serves files from the embedded web/ directory.
var webAssets = func() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal("web: embed sub: ", err)
	}
	return http.FileServer(http.FS(sub))
}()

// handleWeb serves the chat front end and its helper API.
// Returns true if the request was handled.
//
//	GET /            -> web/index.html
//	GET /style.css   -> web/style.css
//	GET /app.js      -> web/app.js
//	GET /web/models  -> candidate models for the picker ("recommended" first)
func handleWeb(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost && r.URL.Path == "/web/mcp" {
		handleMCPWeb(w, r)
		return true
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	switch r.URL.Path {
	case "/web/mcp":
		serveMCPWeb(w, r.Context())
		return true
	case "/web/models":
		serveWebModels(w, r.Context())
		return true
	case "/", "/index.html", "/style.css", "/app.js", "/manifest.json", "/icon-192.png", "/icon-512.png", "/theme-sync.js":
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/index.html" {
			r.URL.Path = "/" // FileServer redirects /index.html -> ./
		}
		webAssets.ServeHTTP(w, r)
		return true
	}
	return false
}

func serveMCPWeb(w http.ResponseWriter, ctx context.Context) {
	servers := GlobalRegistry.Discover()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(servers)
}

func handleMCPWeb(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string    `json:"action"`
		Config MCPConfig `json:"config"`
		Name   string    `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	switch req.Action {
	case "add":
		if req.Config.Name == "" {
			http.Error(w, "Name is required", http.StatusBadRequest)
			return
		}
		if err := GlobalRegistry.AddServer(req.Config); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "remove":
		if req.Name == "" {
			http.Error(w, "Name is required", http.StatusBadRequest)
			return
		}
		GlobalRegistry.RemoveServer(req.Name)
	default:
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

type webModel struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func serveWebModels(w http.ResponseWriter, ctx context.Context) {
	out := []webModel{{
		Name:        "recommended",
		Description: "Let the router pick the best model for each message",
	}}

	models, err := fetchModels(ctx)
	if err != nil {
		log.Printf("web: fetching models failed: %v", err)
	} else {
		for _, m := range models {
			isRouter := false
			for _, c := range m.Capabilities {
				if c == "decision" {
					isRouter = true
					break
				}
			}
			if isRouter {
				continue
			}
			out = append(out, webModel{Name: m.Name, Description: describeModel(m)})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"models":   out,
		"fallback": fallbackModel,
	})
}
