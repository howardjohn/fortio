// Copyright 2025 Fortio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcprunner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fortio.org/fortio/periodic"
)

// testMCPServer creates a simple test MCP server.
func testMCPServer() *httptest.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "initialize":
			resp := JSONRPCResponse{
				JSONRPC: "2.0",
				Result: json.RawMessage(`{
					"protocolVersion": "2024-11-05",
					"capabilities": {"tools": {}},
					"serverInfo": {"name": "test-server", "version": "1.0"}
				}`),
				ID: req.ID,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "notifications/initialized":
			w.WriteHeader(http.StatusNoContent)

		case "tools/call":
			resp := JSONRPCResponse{
				JSONRPC: "2.0",
				Result: json.RawMessage(`{
					"content": [{"type": "text", "text": "success"}]
				}`),
				ID: req.ID,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		default:
			resp := JSONRPCResponse{
				JSONRPC: "2.0",
				Error: &JSONRPCError{
					Code:    -32601,
					Message: fmt.Sprintf("Method not found: %s", req.Method),
				},
				ID: req.ID,
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(resp)
		}
	})

	return httptest.NewServer(handler)
}

func TestNewMCPClient(t *testing.T) {
	opts := &MCPOptions{
		URL:      "http://localhost:8889",
		Tool:     "test_tool",
		Args:     `{"key": "value"}`,
		Sessions: 2,
	}

	client, err := NewMCPClient(opts)
	if err != nil {
		t.Fatalf("Failed to create MCP client: %v", err)
	}

	if client.url != opts.URL {
		t.Errorf("Expected URL %s, got %s", opts.URL, client.url)
	}

	if client.tool != opts.Tool {
		t.Errorf("Expected tool %s, got %s", opts.Tool, client.tool)
	}

	if client.sessions != opts.Sessions {
		t.Errorf("Expected sessions %d, got %d", opts.Sessions, client.sessions)
	}

	expectedArgs := map[string]interface{}{"key": "value"}
	if client.args["key"] != expectedArgs["key"] {
		t.Errorf("Expected args to be parsed correctly")
	}
}

func TestNewMCPClientInvalidJSON(t *testing.T) {
	opts := &MCPOptions{
		URL:      "http://localhost:8889",
		Tool:     "test_tool",
		Args:     `{invalid json}`,
		Sessions: 1,
	}

	_, err := NewMCPClient(opts)
	if err == nil {
		t.Fatal("Expected error for invalid JSON args, got nil")
	}
}

func TestMCPClientInitialize(t *testing.T) {
	server := testMCPServer()
	defer server.Close()

	opts := &MCPOptions{
		URL:      server.URL,
		Tool:     "test_tool",
		Args:     `{}`,
		Sessions: 2,
	}

	client, err := NewMCPClient(opts)
	if err != nil {
		t.Fatalf("Failed to create MCP client: %v", err)
	}

	err = client.Initialize(0)
	if err != nil {
		t.Fatalf("Failed to initialize client: %v", err)
	}

	if len(client.sessionIDs) != opts.Sessions {
		t.Errorf("Expected %d session IDs, got %d", opts.Sessions, len(client.sessionIDs))
	}
}

func TestMCPClientCallTool(t *testing.T) {
	server := testMCPServer()
	defer server.Close()

	opts := &MCPOptions{
		URL:      server.URL,
		Tool:     "test_tool",
		Args:     `{"param": "value"}`,
		Sessions: 1,
	}

	client, err := NewMCPClient(opts)
	if err != nil {
		t.Fatalf("Failed to create MCP client: %v", err)
	}

	err = client.Initialize(0)
	if err != nil {
		t.Fatalf("Failed to initialize client: %v", err)
	}

	err = client.CallTool()
	if err != nil {
		t.Errorf("Failed to call tool: %v", err)
	}
}

func TestRunMCPTest(t *testing.T) {
	server := testMCPServer()
	defer server.Close()

	opts := &RunnerOptions{
		RunnerOptions: periodic.RunnerOptions{
			QPS:        10,
			Duration:   1 * time.Second,
			NumThreads: 1,
		},
		MCPOptions: MCPOptions{
			URL:      server.URL,
			Tool:     "test_tool",
			Args:     `{"test": "data"}`,
			Sessions: 1,
		},
	}

	result, err := RunMCPTest(opts)
	if err != nil {
		t.Fatalf("RunMCPTest failed: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if result.RetCodes["OK"] == 0 {
		t.Errorf("Expected some successful calls, got 0")
	}

	totalCalls := int64(0)
	for _, count := range result.RetCodes {
		totalCalls += count
	}

	if totalCalls == 0 {
		t.Error("Expected at least one call to be made")
	}
}

func TestMCPURLPrefix(t *testing.T) {
	if MCPURLPrefix != "mcp://" {
		t.Errorf("Expected MCPURLPrefix to be 'mcp://', got '%s'", MCPURLPrefix)
	}
}
