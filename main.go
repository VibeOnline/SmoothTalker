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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"
)

const (
	target          = "http://127.0.0.1:11434"
	listenAddr      = ":1337"
	fallbackModel   = "gemma4:e2b-mlx"
	maxBodyBytes    = 10 << 20 // 10 MiB
	maxUpstreamBody = 64 << 20
	maxToolRounds   = 8
	modelCacheTTL   = 30 * time.Second
	mcpToolTimeout  = 10 * time.Second
)

type chatOpts struct {
	luaTools bool // expose and execute Lua tool scripts (web frontend only)
}

// Side calls (tags + router): bounded timeout.
var client = &http.Client{Timeout: 60 * time.Second}

// Chat calls to Ollama: generations can run for minutes, rely on request context instead.
var upstream = &http.Client{}

var discoverTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "mcp_discover",
		"description": "Lists all active MCP servers and their configured commands. Use this tool first to find which external tools are available. To call a tool on a specific server, use the format 'server_name.tool_name' as the function name.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
}

//go:embed RouterSystemPrompt.md
var routerSystemPrompt string

type ollamaModelDetails struct {
	ParameterSize     string `json:"parameter_size"`
	QuantizationLevel string `json:"quantization_level"`
	Runner            string `json:"runner"`
	ContextLength     int    `json:"context_length"`
}

type ollamaModel struct {
	Name         string             `json:"name"`
	RemoteModel  string             `json:"remote_model"`
	RemoteHost   string             `json:"remote_host"`
	Capabilities []string           `json:"capabilities"`
	Details      ollamaModelDetails `json:"details"`
}

type ollamaModelList struct {
	Models []ollamaModel `json:"models"`
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
	runMCPCoreTest()
	runMCPRegistryTest()
	runMCPDiscoverTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	InitRegistry(ctx)
	defer GlobalRegistry.Shutdown()
	InitLuaTools()

	remote, err := url.Parse(target)
	if err != nil {
		log.Fatal("Invalid target URL: ", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(remote)

	uiMux := http.NewServeMux()
	registerToolAdmin(uiMux)
	registerModelAdmin(uiMux)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Frontend-only chat endpoint: Lua tools enabled.
		if r.Method == http.MethodPost && r.URL.Path == "/ui/api/chat" {
			r.URL.Path = "/api/chat" // upstream path for callUpstream
			handleChat(w, r, chatOpts{luaTools: true})
			return
		}

		if strings.HasPrefix(r.URL.Path, "/ui/api/tools") || strings.HasPrefix(r.URL.Path, "/ui/api/models") {
			uiMux.ServeHTTP(w, r)
			return
		}

		if handleWeb(w, r) {
			return
		}

		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
			return
		}

		if r.Method == http.MethodPost {
			switch r.URL.Path {
			case "/api/chat", "/v1/chat/completions":
				handleChat(w, r, chatOpts{}) // no Lua tools
				return
			case "/api/generate":
				req, ok := prepareRequest(w, r)
				if !ok {
					return
				}
				if !setBody(w, r, req) {
					return
				}
				proxy.ServeHTTP(w, r)
				return
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

// ---------- chat handling with MCP tool loop ----------

func handleChat(w http.ResponseWriter, r *http.Request, opts chatOpts) {
	req, ok := prepareRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	isOpenAI := r.URL.Path == "/v1/chat/completions"
	wantStream := streamRequested(req, isOpenAI)

	injectMCPTools(ctx, req, opts)

	// Always talk to Ollama non-streaming so we can inspect tool calls;
	// we convert back to the client's requested format at the end.
	req["stream"] = false

	for round := 0; round < maxToolRounds; round++ {
		status, ctype, body, err := callUpstream(ctx, r, req)
		if err != nil {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
			return
		}
		if status != http.StatusOK {
			w.Header().Set("Content-Type", ctype)
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}

		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			log.Printf("non-JSON upstream response: %v", err)
			w.Header().Set("Content-Type", ctype)
			_, _ = w.Write(body)
			return
		}

		msg := assistantMessage(resp, isOpenAI)
		calls := toolCallsOf(msg)
		if len(calls) == 0 || !allServerCalls(calls, opts) {
			// Either a normal answer or the client's own tools: hand it back.
			writeFinal(w, body, resp, isOpenAI, wantStream)
			return
		}

		// Execute MCP tool calls and feed results back to the model.
		msgs, _ := req["messages"].([]any)
		msgs = append(msgs, msg)
		for _, call := range calls {
			name, _ := toolCallNameArgs(call)
			result := dispatchToolCall(ctx, call, opts)
			log.Printf("tool %s -> %d bytes", name, len(result))

			toolMsg := map[string]any{"role": "tool", "content": result}
			if isOpenAI {
				toolMsg["tool_call_id"] = call["id"]
			} else {
				toolMsg["tool_name"] = name
			}
			msgs = append(msgs, toolMsg)
		}
		req["messages"] = msgs
	}

	http.Error(w, "too many tool-call rounds", http.StatusBadGateway)
}

func streamRequested(req map[string]any, isOpenAI bool) bool {
	if s, ok := req["stream"].(bool); ok {
		return s
	}
	return !isOpenAI // Ollama defaults to streaming, OpenAI does not
}

func callUpstream(ctx context.Context, orig *http.Request, req map[string]any) (int, string, []byte, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return 0, "", nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, target+orig.URL.Path, bytes.NewReader(payload))
	if err != nil {
		return 0, "", nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if auth := orig.Header.Get("Authorization"); auth != "" {
		hr.Header.Set("Authorization", auth)
	}
	resp, err := upstream.Do(hr)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil {
		return 0, "", nil, err
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/json"
	}
	return resp.StatusCode, ctype, body, nil
}

func assistantMessage(resp map[string]any, isOpenAI bool) map[string]any {
	if isOpenAI {
		if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
			if c, ok := choices[0].(map[string]any); ok {
				if m, ok := c["message"].(map[string]any); ok {
					return m
				}
			}
		}
		return nil
	}
	m, _ := resp["message"].(map[string]any)
	return m
}

func toolCallsOf(msg map[string]any) []map[string]any {
	if msg == nil {
		return nil
	}
	raw, _ := msg["tool_calls"].([]any)
	var out []map[string]any
	for _, t := range raw {
		if tm, ok := t.(map[string]any); ok {
			out = append(out, tm)
		}
	}
	return out
}

// isServerCall reports whether the proxy should execute this call itself.
func isServerCall(call map[string]any, opts chatOpts) bool {
	name, _, ok := extractToolCall(call)
	if !ok {
		return false
	}
	if opts.luaTools && LuaTools.Has(name) {
		return true
	}
	if name == "mcp_discover" {
		return true
	}
	parts := strings.SplitN(name, ".", 2)
	if len(parts) != 2 || GlobalRegistry == nil {
		return false
	}
	_, err := GlobalRegistry.GetClient(parts[0])
	return err == nil
}

func allServerCalls(calls []map[string]any, opts chatOpts) bool {
	for _, c := range calls {
		if !isServerCall(c, opts) {
			return false
		}
	}
	return true
}

// writeFinal emits the non-streaming upstream body in whatever shape the client asked for.
func writeFinal(w http.ResponseWriter, body []byte, resp map[string]any, isOpenAI, wantStream bool) {
	w.Header().Del("Content-Length")

	if !wantStream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}

	if !isOpenAI {
		// A single Ollama non-stream object is a valid final NDJSON chunk (done:true).
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write(append(body, '\n'))
		return
	}

	// OpenAI: convert chat.completion -> one chat.completion.chunk + [DONE].
	msg := assistantMessage(resp, true)
	delta := map[string]any{"role": "assistant"}
	if msg != nil {
		if c, ok := msg["content"]; ok && c != nil {
			delta["content"] = c
		}
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			for i, t := range tc {
				if tm, ok := t.(map[string]any); ok {
					tm["index"] = i
				}
			}
			delta["tool_calls"] = tc
		}
	}
	finish := "stop"
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
		}
	}
	chunk := map[string]any{
		"id":      resp["id"],
		"object":  "chat.completion.chunk",
		"created": resp["created"],
		"model":   resp["model"],
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if u, ok := resp["usage"]; ok {
		chunk["usage"] = u
	}
	data, _ := json.Marshal(chunk)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

// ---------- tool injection ----------

// injectMCPTools adds mcp_discover plus every tool exposed by connected MCP
// servers (as "server.tool"), but only if the request already has tools or
// the target model advertises tool support.
func injectMCPTools(ctx context.Context, req map[string]any, opts chatOpts) {
	tools, hasTools := req["tools"].([]any)
	model, _ := req["model"].(string)
	if !hasTools && !modelSupportsTools(ctx, model) {
		return
	}

	existing := map[string]bool{}
	for _, t := range tools {
		if tm, ok := t.(map[string]any); ok {
			if fn, ok := tm["function"].(map[string]any); ok {
				if n, ok := fn["name"].(string); ok {
					existing[n] = true
				}
			}
		}
	}

	add := func(t map[string]any) {
		name := t["function"].(map[string]any)["name"].(string)
		if !existing[name] {
			tools = append(tools, t)
			existing[name] = true
		}
	}
	add(discoverTool)
	for _, t := range mcpToolDefinitions(ctx) {
		add(t)
	}
	req["tools"] = tools

	add(discoverTool)
	if opts.luaTools {
		for _, t := range LuaTools.Definitions() {
			add(t)
		}
	}
	for _, t := range mcpToolDefinitions(ctx) {
		add(t)
	}
	req["tools"] = tools
}

var mcpToolCache struct {
	sync.Mutex
	at    time.Time
	tools []map[string]any
}

// mcpToolDefinitions converts MCP tools/list results into OpenAI-style tool definitions.
func mcpToolDefinitions(_ context.Context) []map[string]any {
	mcpToolCache.Lock()
	defer mcpToolCache.Unlock()
	if time.Since(mcpToolCache.at) < modelCacheTTL {
		return mcpToolCache.tools
	}

	var out []map[string]any
	if GlobalRegistry != nil {
		for server := range GlobalRegistry.Discover() {
			c, err := GlobalRegistry.GetClient(server)
			if err != nil {
				continue
			}
			raw, err := c.ListTools(mcpToolTimeout)
			if err != nil {
				log.Printf("tools/list failed for %s: %v", server, err)
				continue
			}
			var list struct {
				Tools []struct {
					Name        string         `json:"name"`
					Description string         `json:"description"`
					InputSchema map[string]any `json:"inputSchema"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				continue
			}
			for _, t := range list.Tools {
				params := t.InputSchema
				if params == nil {
					params = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				out = append(out, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        server + "." + t.Name,
						"description": t.Description,
						"parameters":  params,
					},
				})
			}
		}
	}
	mcpToolCache.at = time.Now()
	mcpToolCache.tools = out
	return out
}

var modelCache struct {
	sync.Mutex
	at     time.Time
	models []ollamaModel
}

func cachedModels(ctx context.Context) ([]ollamaModel, error) {
	modelCache.Lock()
	defer modelCache.Unlock()
	if time.Since(modelCache.at) < modelCacheTTL && modelCache.models != nil {
		return modelCache.models, nil
	}
	models, err := fetchModels(ctx)
	if err != nil {
		return modelCache.models, err
	}
	modelCache.at = time.Now()
	modelCache.models = models
	return models, nil
}

func modelSupportsTools(ctx context.Context, name string) bool {
	models, err := cachedModels(ctx)
	if err != nil || models == nil {
		return false
	}
	for _, m := range models {
		if m.Name == name || strings.TrimSuffix(m.Name, ":latest") == name {
			for _, c := range m.Capabilities {
				if c == "tools" {
					return true
				}
			}
			return false
		}
	}
	return false
}

// ---------- request preparation ----------

// prepareRequest reads and parses the body, resolves "recommended", and
// injects options. It does NOT write the body back (see setBody).
func prepareRequest(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	r.Body.Close()
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Error reading request body", http.StatusBadRequest)
		}
		return nil, false
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return nil, false
	}

	// Routing must happen BEFORE tool injection so requestHints sees the
	// client's real tool usage.
	if model, _ := req["model"].(string); model == "recommended" {
		req["model"] = chooseModel(r.Context(), req, r.URL.Path)
	}

	// NOTE: Stock Ollama uses a top-level "think" field; keep this only if
	// your Ollama build reads options.thinking_level.
	options, ok := req["options"].(map[string]any)
	if !ok {
		options = make(map[string]any)
		req["options"] = options
	}
	options["thinking_level"] = 5

	return req, true
}

func setBody(w http.ResponseWriter, r *http.Request, req map[string]any) bool {
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

// ---------- routing (unchanged logic) ----------

func chooseModel(ctx context.Context, req map[string]any, path string) string {
	log.Printf("Model is 'recommended'. Determining best model via /v1/systemone...")

	models, err := cachedModels(ctx)
	if err != nil || models == nil {
		log.Printf("Error fetching models: %v. Falling back to %s.", err, fallbackModel)
		return fallbackModel
	}

	isDecision := func(m ollamaModel) bool {
		for _, c := range m.Capabilities {
			if c == "decision" {
				return true
			}
		}
		return false
	}

	var currentRouter *ollamaModel
	for i := range models {
		if isDecision(models[i]) {
			currentRouter = &models[i]
			break
		}
	}
	if currentRouter == nil {
		log.Printf("No model with 'decision' capability found. Falling back to %s.", fallbackModel)
		return fallbackModel
	}

	candidates := make(map[string]string)
	for _, m := range models {
		if !isDecision(m) {
			candidates[m.Name] = describeModel(m)
		}
	}
	if len(candidates) == 0 {
		log.Printf("No candidate models available. Falling back to %s.", fallbackModel)
		return fallbackModel
	}

	state := extractInput(req, path, currentRouter)
	if hint := requestHints(req); hint != "" {
		if state == "Empty request" {
			state = hint
		} else {
			state += "\n\n" + hint
		}
	}

	choice, err := askRouter(ctx, state, candidates, currentRouter.Name)
	if err != nil {
		log.Printf("Router failed: %v. Falling back to %s.", err, fallbackModel)
		return fallbackModel
	}
	if _, valid := candidates[choice]; !valid {
		log.Printf("Router chose unknown model %q. Falling back to %s.", choice, fallbackModel)
		return fallbackModel
	}

	log.Printf("Router (%s) chose model: %s", currentRouter.Name, choice)
	return choice
}

func fetchModels(ctx context.Context) ([]ollamaModel, error) {
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
	return list.Models, nil
}

func askRouter(ctx context.Context, input string, candidates map[string]string, routerModel string) (string, error) {
	payload := map[string]any{
		"model": routerModel,
		"state": input,
		"questions": map[string]any{
			"best_model": map[string]any{
				"type":         "choice",
				"instructions": routerSystemPrompt,
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

func (m ollamaModel) String() string {
	return describeModelInternal(m)
}

func describeModel(m ollamaModel) string {
	return m.String()
}

func describeModelInternal(m ollamaModel) string {
	var b strings.Builder
	if m.RemoteHost != "" {
		host := m.RemoteHost
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
		name := m.RemoteModel
		if name == "" {
			name = m.Name
		}
		fmt.Fprintf(&b, "Cloud model %s at %s — paid per token, slower and costlier than local", name, host)
	} else {
		b.WriteString("Local model — free and fastest, runs on-device")
	}
	if m.Details.Runner != "" {
		fmt.Fprintf(&b, " (%s)", m.Details.Runner)
	}
	if m.Details.ParameterSize != "" {
		fmt.Fprintf(&b, "; %s parameters", m.Details.ParameterSize)
	}
	if m.Details.QuantizationLevel != "" {
		fmt.Fprintf(&b, "; %s quantization", m.Details.QuantizationLevel)
	}
	if m.Details.ContextLength > 0 {
		fmt.Fprintf(&b, "; %s context window", humanContext(m.Details.ContextLength))
	}
	if len(m.Capabilities) > 0 {
		fmt.Fprintf(&b, "; supports %s", strings.Join(m.Capabilities, ", "))
	}
	return b.String()
}

func humanContext(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func requestHints(req map[string]any) string {
	var hints []string

	hasImages := false
	if msgs, ok := req["messages"].([]any); ok {
		for _, mm := range msgs {
			if m, ok := mm.(map[string]any); ok {
				if imgs, ok := m["images"].([]any); ok && len(imgs) > 0 {
					hasImages = true
				}
			}
		}
	}
	if !hasImages {
		if imgs, ok := req["images"].([]any); ok && len(imgs) > 0 {
			hasImages = true
		}
	}
	if hasImages {
		hints = append(hints, "The request contains image attachments, so the chosen model must support vision.")
	}
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		hints = append(hints, "The request includes tool definitions, so the chosen model must support tool calling.")
	}
	return strings.Join(hints, "\n")
}

var (
	stripTagRes = func() []*regexp.Regexp {
		var res []*regexp.Regexp
		for _, tag := range []string{"tools", "rules", "docs", "skills", "cwd", "acp", "available_skills"} {
			res = append(res, regexp.MustCompile("(?is)<"+tag+`[^>]*>.*?</`+tag+`>`))
		}
		return res
	}()
	reAutomated = regexp.MustCompile(`(?is)\[.*?automated context.*?\]`)
)

func extractInput(req map[string]any, path string, router *ollamaModel) string {
	var parts []string
	if path == "/api/chat" || path == "/v1/chat/completions" {
		if msgs, ok := req["messages"].([]any); ok {
			var lastUser string
			for _, mm := range msgs {
				m, ok := mm.(map[string]any)
				if !ok {
					continue
				}
				role, _ := m["role"].(string)
				content, _ := m["content"].(string)
				if role == "user" && content != "" {
					lastUser = content
				}
			}
			if lastUser != "" {
				parts = append(parts, lastUser)
			}
		}
	} else if prompt, _ := req["prompt"].(string); prompt != "" {
		parts = append(parts, prompt)
	}

	res := strings.Join(parts, "\n\n")
	if res == "" {
		return "Empty request"
	}

	for _, re := range stripTagRes {
		res = re.ReplaceAllString(res, "")
	}
	for _, s := range []string{
		"--- pi-extension-context:start ---", "--- pi-extension-context:end ---",
		"<file-context>", "</file-context>",
		"📌 pi-lens active",
		"Key tools (see each tool's own description for args):",
	} {
		res = strings.ReplaceAll(res, s, "")
	}
	res = reAutomated.ReplaceAllString(res, "")

	res = strings.TrimSpace(res)
	if res == "" {
		return "Empty request"
	}

	maxTokens := 2048
	if router != nil && router.Details.ContextLength > 0 {
		maxTokens = router.Details.ContextLength
	}
	effectiveMaxTokens := maxTokens - 256
	if effectiveMaxTokens < 1 {
		effectiveMaxTokens = 1
	}
	maxChars := effectiveMaxTokens * 3

	if len(res) > maxChars {
		const truncatedText = "... [truncated]"
		return res[:maxChars-len(truncatedText)] + truncatedText
	}
	return res
}
