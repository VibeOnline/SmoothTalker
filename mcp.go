package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// MCP JSON-RPC Types
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// MCP Client represents a connection to an MCP server.
type MCPClient struct {
	name     string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	pending  map[interface{}]chan *JSONRPCResponse
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	isClosed bool
}

// MCPConfig defines how to start an MCP server.
type MCPConfig struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// MCPRegistry manages active MCP clients and their configurations.
type MCPRegistry struct {
	configs map[string]MCPConfig
	clients map[string]*MCPClient
	mu      sync.RWMutex
	ctx     context.Context
}

var GlobalRegistry *MCPRegistry

func InitRegistry(ctx context.Context) {
	GlobalRegistry = NewMCPRegistry(ctx)
}

func NewMCPRegistry(ctx context.Context) *MCPRegistry {
	return &MCPRegistry{
		configs: make(map[string]MCPConfig),
		clients: make(map[string]*MCPClient),
		ctx:     ctx,
	}
}

func (r *MCPRegistry) AddServer(cfg MCPConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if strings.ContainsAny(cfg.Command, ";&|`$") {
		return fmt.Errorf("invalid command: contains forbidden characters")
	}

	r.configs[cfg.Name] = cfg
	return r.startClient(cfg)
}

func (r *MCPRegistry) startClient(cfg MCPConfig) error {
	client, err := NewMCPClient(r.ctx, cfg.Name, cfg.Command, cfg.Args)
	if err != nil {
		return err
	}
	r.clients[cfg.Name] = client
	return nil
}

func (r *MCPRegistry) RemoveServer(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if client, ok := r.clients[name]; ok {
		client.Close()
		delete(r.clients, name)
	}
	delete(r.configs, name)
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

// Discover retrieves a list of all configured MCP servers and their metadata.
func (r *MCPRegistry) Discover() map[string]MCPConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	res := make(map[string]MCPConfig)
	for name, cfg := range r.configs {
		res[name] = cfg
	}
	return res
}

func NewMCPClient(ctx context.Context, name string, command string, args []string) (*MCPClient, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, command, args...)

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

	client := &MCPClient{
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		pending: make(map[interface{}]chan *JSONRPCResponse),
		ctx:     ctx,
		cancel:  cancel,
	}

	go client.readLoop()

	return client, nil
}

func (c *MCPClient) readLoop() {
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("[%s] Read error: %v", c.name, err)
			}
			c.Close()
			return
		}

		var resp JSONRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			log.Printf("[%s] Failed to unmarshal response: %v", c.name, err)
			continue
		}

		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			ch <- &resp
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
	}
}

func (c *MCPClient) Call(method string, params any, timeout time.Duration) (*JSONRPCResponse, error) {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil, fmt.Errorf("client closed")
	}

	id := time.Now().UnixNano()
	ch := make(chan *JSONRPCResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  nil,
	}
	if params != nil {
		pBytes, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		req.Params = pBytes
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if _, err := c.stdin.Write(append(reqBytes, '\n')); err != nil {
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("request timed out")
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

func (c *MCPClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return
	}
	c.isClosed = true
	c.cancel()
	c.stdin.Close()
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
}
