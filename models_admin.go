package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// ---------- Model Browser API ----------

func registerModelAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/api/models", modelsList)
	mux.HandleFunc("POST /ui/api/models/pull", sameOrigin(modelsPull))
	mux.HandleFunc("DELETE /ui/api/models/{name}", sameOrigin(modelsDelete))
}

func modelsList(w http.ResponseWriter, r *http.Request) {
	models, err := fetchModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to fetch models: "+err.Error())
		return
	}

	var modelItems []struct {
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		Capabilities []string `json:"capabilities"`
	}

	for _, m := range models {
		modelItems = append(modelItems, struct {
			Name         string   `json:"name"`
			Description  string   `json:"description"`
			Capabilities []string `json:"capabilities"`
		}{
			Name:         m.Name,
			Description:  describeModelInternal(m),
			Capabilities: m.Capabilities,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"models": modelItems})
}

func modelsPull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Model == "" {
		writeErr(w, http.StatusBadRequest, "model name is required")
		return
	}

	payload := map[string]string{"name": body.Model}
	pBody, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, target+"/api/pull", bytes.NewReader(pBody))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := upstream.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "ollama pull failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		writeErr(w, resp.StatusCode, "ollama: "+string(body))
		return
	}

	// Proxy the response body (which contains the progress updates) to the client.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			writeErr(w, http.StatusInternalServerError, "stream error: "+err.Error())
			return
		}
	}
}

func modelsDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "model name is required")
		return
	}

	// Some Ollama versions use /api/delete instead of /api/tags/{name}
	// or require the name in a JSON body for DELETE.
	// However, the most common current API for deleting a specific tag is DELETE /api/tags/{name}.
	// If we are getting 404s consistently, let's try the alternate DELETE /api/delete approach
	// with a JSON body if the tag-based one fails.

	req, err := http.NewRequest(http.MethodDelete, target+"/api/tags/"+name, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp, err := upstream.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "ollama delete failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Try the alternative: DELETE /api/delete with {"name": "..."}
		payload := map[string]string{"name": name}
		pBody, _ := json.Marshal(payload)

		reqAlt, errAlt := http.NewRequest(http.MethodDelete, target+"/api/delete", bytes.NewReader(pBody))
		if errAlt == nil {
			reqAlt.Header.Set("Content-Type", "application/json")
			respAlt, errAlt := upstream.Do(reqAlt)
			if errAlt == nil {
				defer respAlt.Body.Close()
				if respAlt.StatusCode == http.StatusOK || respAlt.StatusCode == http.StatusNoContent {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
		}
		writeErr(w, http.StatusNotFound, "ollama: model not found on both endpoints")
		return
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		writeErr(w, resp.StatusCode, "ollama: "+string(body))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
