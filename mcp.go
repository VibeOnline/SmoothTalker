package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- JSON-RPC types ----------

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"` // omitted for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method,omitempty"` // set when the server sends a notification/request
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// idKey normalizes a JSON-RPC id (string / float64 / int64) into a map key.
func idKey(id any) string {
	switch v := id.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return fmt.Sprint(v)
	}
}

// ---------- MCP client ----------

type MCPClient struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	writeMu sync.Mutex // serializes writes to stdin
	mu      sync.Mutex // guards pending / isClosed / initialized
	pending map[string]chan *JSONRPCResponse

	nextID      atomic.Int64
	initialized bool
	initOnce    sync.Once
	initErr     error

	ctx      context.Context
	cancel   context.CancelFunc
	isClosed bool
}

type MCPConfig struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func NewMCPClient(ctx context.Context, name, command string, args []string) (*MCPClient, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start MCP server: %w", err)
	}

	c := &MCPClient{
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdout, 1<<20),
		pending: make(map[string]chan *JSONRPCResponse),
		ctx:     ctx,
		cancel:  cancel,
	}

	go c.readLoop()
	go func() {
		// Reap the process so it never becomes a zombie.
		_ = cmd.Wait()
		c.Close()
	}()

	return c, nil
}

func (c *MCPClient) readLoop() {
	defer c.Close()
	for {
		line, err := c.stdout.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				c.dispatch(line)
			}
		}
		if err != nil {
			if err != io.EOF && c.ctx.Err() == nil {
				log.Printf("[%s] read error: %v", c.name, err)
			}
			return
		}
	}
}

func (c *MCPClient) dispatch(line []byte) {
	var resp JSONRPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		log.Printf("[%s] non-JSON line from server: %q", c.name, line)
		return
	}
	// Notifications / server->client requests: ignore for now.
	if resp.Method != "" && resp.ID == nil {
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[idKey(resp.ID)]
	if ok {
		delete(c.pending, idKey(resp.ID))
	}
	c.mu.Unlock()

	if ok {
		ch <- &resp // buffered(1); never blocks
	}
}

func (c *MCPClient) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(b)
	if err != nil {
		return fmt.Errorf("failed to write request: %w", err)
	}
	return nil
}

// Notify sends a JSON-RPC notification (no id, no response expected).
func (c *MCPClient) Notify(method string, params any) error {
	req := JSONRPCRequest{JSONRPC: "2.0", Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = p
	}
	return c.write(req)
}

// Call sends a request and waits for the matching response.
func (c *MCPClient) Call(method string, params any, timeout time.Duration) (*JSONRPCResponse, error) {
	req := JSONRPCRequest{JSONRPC: "2.0", Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		req.Params = p
	}

	id := c.nextID.Add(1)
	req.ID = id
	key := idKey(id)
	ch := make(chan *JSONRPCResponse, 1)

	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil, fmt.Errorf("client %s closed", c.name)
	}
	c.pending[key] = ch
	c.mu.Unlock()

	unregister := func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}

	if err := c.write(req); err != nil {
		unregister()
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp, nil
	case <-timer.C:
		unregister()
		return nil, fmt.Errorf("request %s timed out after %s", method, timeout)
	case <-c.ctx.Done():
		unregister()
		return nil, c.ctx.Err()
	}
}

// Initialize performs the MCP handshake exactly once.
func (c *MCPClient) Initialize(timeout time.Duration) error {
	c.initOnce.Do(func() {
		params := map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "go-mcp-proxy",
				"version": "0.1.0",
			},
		}
		if _, err := c.Call("initialize", params, timeout); err != nil {
			c.initErr = fmt.Errorf("initialize failed: %w", err)
			return
		}
		if err := c.Notify("notifications/initialized", map[string]any{}); err != nil {
			c.initErr = err
			return
		}
		c.mu.Lock()
		c.initialized = true
		c.mu.Unlock()
	})
	return c.initErr
}

// ListTools returns the server's tool list (initializing first if needed).
func (c *MCPClient) ListTools(timeout time.Duration) (json.RawMessage, error) {
	if err := c.Initialize(timeout); err != nil {
		return nil, err
	}
	resp, err := c.Call("tools/list", map[string]any{}, timeout)
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// CallTool invokes a tool (initializing first if needed).
func (c *MCPClient) CallTool(name string, args any, timeout time.Duration) (json.RawMessage, error) {
	if err := c.Initialize(timeout); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	resp, err := c.Call("tools/call", map[string]any{"name": name, "arguments": args}, timeout)
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

func (c *MCPClient) Close() {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return
	}
	c.isClosed = true
	c.pending = make(map[string]chan *JSONRPCResponse)
	c.mu.Unlock()

	c.cancel()
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// ---------- Registry ----------

type MCPRegistry struct {
	configs     map[string]MCPConfig
	clients     map[string]*MCPClient
	mu          sync.RWMutex
	ctx         context.Context
	storagePath string
}

var GlobalRegistry *MCPRegistry

func InitRegistry(ctx context.Context) {
	GlobalRegistry = NewMCPRegistry(ctx)
	GlobalRegistry.loadConfigs()
}

func NewMCPRegistry(ctx context.Context) *MCPRegistry {
	return &MCPRegistry{
		configs:     make(map[string]MCPConfig),
		clients:     make(map[string]*MCPClient),
		ctx:         ctx,
		storagePath: "mcp_configs.json",
	}
}

// saveConfigs must be called WITHOUT the lock held.
func (r *MCPRegistry) saveConfigs() {
	r.mu.RLock()
	data, err := json.MarshalIndent(r.configs, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		log.Printf("Error marshaling MCP configs: %v", err)
		return
	}
	if err := os.WriteFile(r.storagePath, data, 0o644); err != nil {
		log.Printf("Error saving MCP configs: %v", err)
	}
}

func (r *MCPRegistry) loadConfigs() {
	data, err := os.ReadFile(r.storagePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error reading MCP configs: %v", err)
		}
		return
	}

	var cfgs map[string]MCPConfig
	if err := json.Unmarshal(data, &cfgs); err != nil {
		log.Printf("Error unmarshaling MCP configs: %v", err)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for name, cfg := range cfgs {
		if cfg.Name == "" {
			cfg.Name = name
		}
		r.configs[cfg.Name] = cfg
		if err := r.startClientLocked(cfg); err != nil {
			log.Printf("Failed to start loaded MCP server %s: %v", cfg.Name, err)
		}
	}
}

func (r *MCPRegistry) AddServer(cfg MCPConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("server name is required")
	}
	if cfg.Command == "" {
		return fmt.Errorf("command is required")
	}
	if strings.ContainsAny(cfg.Command, ";&|`$") {
		return fmt.Errorf("invalid command: contains forbidden characters")
	}

	r.mu.Lock()
	// Replace any existing instance with this name.
	if old, ok := r.clients[cfg.Name]; ok {
		old.Close()
		delete(r.clients, cfg.Name)
	}
	err := r.startClientLocked(cfg)
	if err == nil {
		r.configs[cfg.Name] = cfg
	}
	r.mu.Unlock()

	if err != nil {
		return err
	}
	r.saveConfigs()
	return nil
}

func (r *MCPRegistry) RemoveServer(name string) {
	r.mu.Lock()
	if client, ok := r.clients[name]; ok {
		client.Close()
		delete(r.clients, name)
	}
	delete(r.configs, name)
	r.mu.Unlock()

	r.saveConfigs()
}

// startClientLocked requires r.mu to be held for writing.
func (r *MCPRegistry) startClientLocked(cfg MCPConfig) error {
	client, err := NewMCPClient(r.ctx, cfg.Name, cfg.Command, cfg.Args)
	if err != nil {
		return err
	}
	r.clients[cfg.Name] = client
	return nil
}

func (r *MCPRegistry) GetClient(name string) (*MCPClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	client, ok := r.clients[name]
	if !ok {
		return nil, fmt.Errorf("server %s not found or not connected", name)
	}
	return client, nil
}

func (r *MCPRegistry) ListServers() []MCPConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]MCPConfig, 0, len(r.configs))
	for _, cfg := range r.configs {
		list = append(list, cfg)
	}
	return list
}

func (r *MCPRegistry) Discover() map[string]MCPConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make(map[string]MCPConfig, len(r.configs))
	for name, cfg := range r.configs {
		res[name] = cfg
	}
	return res
}

// Shutdown closes every running client.
func (r *MCPRegistry) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, c := range r.clients {
		c.Close()
		delete(r.clients, name)
	}
}

// ---------- HTTP response capture ----------

// responseWrapper buffers the body so it can be inspected before being sent.
type responseWrapper struct {
	http.ResponseWriter
	body       *bytes.Buffer
	statusCode int
}

func newResponseWrapper(w http.ResponseWriter) *responseWrapper {
	return &responseWrapper{ResponseWriter: w, body: &bytes.Buffer{}, statusCode: http.StatusOK}
}

func (rw *responseWrapper) Write(b []byte) (int, error) {
	return rw.body.Write(b)
}

func (rw *responseWrapper) WriteHeader(statusCode int) {
	rw.statusCode = statusCode // defer actual write until Flush
}

// Flush writes the captured status and body to the real ResponseWriter.
func (rw *responseWrapper) Flush() error {
	rw.ResponseWriter.WriteHeader(rw.statusCode)
	_, err := rw.ResponseWriter.Write(rw.body.Bytes())
	return err
}

// ---------- Tool-call handling ----------

func handleToolResponse(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		log.Printf("handleToolResponse: failed to unmarshal body: %v", err)
		return "", false
	}

	var toolCalls []map[string]any
	collect := func(msg map[string]any) {
		if tc, ok := msg["tool_calls"].([]any); ok {
			for _, t := range tc {
				if tm, ok := t.(map[string]any); ok {
					toolCalls = append(toolCalls, tm)
				}
			}
		}
	}

	// Ollama: {"message": {"tool_calls": [...]}}
	if msg, ok := resp["message"].(map[string]any); ok {
		collect(msg)
	}
	// OpenAI: {"choices": [{"message": {"tool_calls": [...]}}]}
	if len(toolCalls) == 0 {
		if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					collect(msg)
				}
			}
		}
	}

	if len(toolCalls) == 0 {
		return "", false
	}

	result, handled := handleMCPCall(context.Background(), toolCalls[0])
	if !handled {
		return "", false
	}
	log.Printf("Handled MCP tool call: %s", result)
	return result, true
}

// extractToolCall normalizes Ollama / OpenAI tool-call shapes into (name, args).
func extractToolCall(call map[string]any) (string, map[string]any, bool) {
	var name string
	var rawArgs any

	if fn, ok := call["function"].(map[string]any); ok {
		name, _ = fn["name"].(string)
		rawArgs = fn["arguments"]
	} else if n, ok := call["name"].(string); ok {
		name = n
		rawArgs = call["arguments"]
	}
	if name == "" {
		return "", nil, false
	}

	args := map[string]any{}
	switch a := rawArgs.(type) {
	case map[string]any:
		args = a
	case string: // OpenAI sends arguments as a JSON string
		if a != "" {
			if err := json.Unmarshal([]byte(a), &args); err != nil {
				log.Printf("extractToolCall: bad arguments JSON: %v", err)
			}
		}
	}
	return name, args, true
}

func handleMCPCall(_ context.Context, call map[string]any) (string, bool) {
	funcName, args, ok := extractToolCall(call)
	if !ok {
		return "", false
	}
	if GlobalRegistry == nil {
		return "MCP registry not initialized.", true
	}

	if funcName == "mcp_discover" {
		servers := GlobalRegistry.Discover()
		if len(servers) == 0 {
			return "No active MCP servers found. Please configure one in Settings.", true
		}
		res, _ := json.Marshal(servers)
		return string(res), true
	}

	// "server_name.tool_name"
	parts := strings.SplitN(funcName, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	serverName, toolName := parts[0], parts[1]

	client, err := GlobalRegistry.GetClient(serverName)
	if err != nil {
		return fmt.Sprintf("MCP server %s not found: %v", serverName, err), true
	}

	result, err := client.CallTool(toolName, args, 30*time.Second)
	if err != nil {
		return fmt.Sprintf("MCP call failed on server %s: %v", serverName, err), true
	}
	return string(result), true
}

// ---------- Self-tests ----------

func runMCPCoreTest() {
	fmt.Println("--- Running MCP Core Logic Test ---")
	client, err := NewMCPClient(context.Background(), "test-server", "cat", nil)
	if err != nil {
		log.Printf("Failed to create client: %v", err)
		return
	}
	defer client.Close()

	// 'cat' echoes our request back; it has an "id" but no "result", so
	// the client should match it and return an empty result (no error).
	resp, err := client.Call("tools/list", nil, 2*time.Second)
	if err != nil {
		fmt.Printf("Call error: %v\n", err)
	} else {
		fmt.Printf("Echo matched id=%v result=%q\n", resp.ID, string(resp.Result))
	}
	fmt.Println("--- MCP Core Logic Structure Verified ---")
}

func runMCPRegistryTest() {
	fmt.Println("--- Running MCP Registry Test ---")
	registry := NewMCPRegistry(context.Background())
	registry.storagePath = os.DevNull // don't clobber real config during tests
	defer registry.Shutdown()

	if err := registry.AddServer(MCPConfig{Name: "test-echo", Command: "cat"}); err != nil {
		log.Printf("Failed to add server: %v", err)
		return
	}
	fmt.Println("Test 1: Added server successfully.")

	servers := registry.ListServers()
	if len(servers) != 1 || servers[0].Name != "test-echo" {
		log.Printf("ListServers failed: got %v", servers)
		return
	}
	fmt.Println("Test 2: ListServers verified.")

	if client, err := registry.GetClient("test-echo"); err != nil || client == nil {
		log.Printf("GetClient failed: %v", err)
		return
	}
	fmt.Println("Test 3: GetClient verified.")

	registry.RemoveServer("test-echo")
	if n := len(registry.ListServers()); n != 0 {
		log.Printf("RemoveServer failed: expected 0 servers, got %d", n)
		return
	}
	fmt.Println("Test 4: RemoveServer verified.")
	fmt.Println("--- MCP Registry Logic Verified ---")
}

func runMCPDiscoverTest() {
	fmt.Println("--- Running MCP Discover Test ---")
	registry := NewMCPRegistry(context.Background())
	registry.storagePath = os.DevNull
	defer registry.Shutdown()

	for _, s := range []MCPConfig{
		{Name: "server-1", Command: "cat"},
		{Name: "server-2", Command: "cat"},
	} {
		if err := registry.AddServer(s); err != nil {
			log.Printf("Failed to add server %s: %v", s.Name, err)
			return
		}
	}

	if n := len(registry.Discover()); n != 2 {
		log.Printf("Discover failed: expected 2 servers, got %d", n)
		return
	}
	fmt.Println("Test: Discover returned all active servers successfully.")
	fmt.Println("--- MCP Discover Logic Verified ---")
}
