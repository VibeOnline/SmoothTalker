package main

import (
	"context"
	"fmt"
	"log"
)

func runMCPDiscoverTest() {
	fmt.Println("--- Running MCP Discover Test ---")
	ctx := context.Background()
	registry := NewMCPRegistry(ctx)

	// Add some test servers
	serversToAdd := []MCPConfig{
		{Name: "server-1", Command: "cat", Args: []string{}},
		{Name: "server-2", Command: "cat", Args: []string{}},
	}

	for _, s := range serversToAdd {
		if err := registry.AddServer(s); err != nil {
			log.Printf("Failed to add server %s: %v", s.Name, err)
			return
		}
	}

	// Test Discover
	discovered := registry.Discover()
	if len(discovered) != 2 {
		log.Printf("Discover failed: expected 2 servers, got %d", len(discovered))
		return
	}

	if _, ok := discovered["server-1"]; !ok {
		log.Printf("Discover failed: server-1 missing")
		return
	}
	if _, ok := discovered["server-2"]; !ok {
		log.Printf("Discover failed: server-2 missing")
		return
	}

	fmt.Println("Test: Discover returned all active servers successfully.")
	fmt.Println("--- MCP Discover Logic Verified ---")
}
