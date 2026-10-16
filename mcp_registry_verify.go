package main

import (
	"context"
	"fmt"
	"log"
)

func runMCPRegistryTest() {
	fmt.Println("--- Running MCP Registry Test ---")
	ctx := context.Background()
	registry := NewMCPRegistry(ctx)

	// Test 1: Add Server
	cfg := MCPConfig{
		Name:    "test-echo",
		Command: "cat",
		Args:    []string{},
	}
	err := registry.AddServer(cfg)
	if err != nil {
		log.Printf("Failed to add server: %v", err)
		return
	}
	fmt.Println("Test 1: Added server successfully.")

	// Test 2: List Servers
	servers := registry.ListServers()
	if len(servers) != 1 || servers[0].Name != "test-echo" {
		log.Printf("ListServers failed: expected 1 server 'test-echo', got %v", servers)
		return
	}
	fmt.Println("Test 2: ListServers verified.")

	// Test 3: Get Client
	client, err := registry.GetClient("test-echo")
	if err != nil || client == nil {
		log.Printf("GetClient failed: %v", err)
		return
	}
	fmt.Println("Test 3: GetClient verified.")

	// Test 4: Remove Server
	registry.RemoveServer("test-echo")
	servers = registry.ListServers()
	if len(servers) != 0 {
		log.Printf("RemoveServer failed: expected 0 servers, got %v", servers)
		return
	}
	fmt.Println("Test 4: RemoveServer verified.")

	fmt.Println("--- MCP Registry Logic Verified ---")
}
