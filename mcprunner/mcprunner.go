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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"fortio.org/fortio/fhttp"
	"fortio.org/fortio/periodic"
	"fortio.org/log"
)

// MCPURLPrefix is the URL prefix for triggering MCP load.
var MCPURLPrefix = "mcp://"

// JSONRPCRequest represents a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
	ID      interface{} `json:"id,omitempty"` // omitted for notifications
}

// JSONRPCResponse represents a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
	ID      interface{}     `json:"id"`
}

// JSONRPCError represents a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// InitializeParams are the parameters for the initialize request.
type InitializeParams struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]interface{} `json:"capabilities"`
	ClientInfo      ClientInfo             `json:"clientInfo"`
}

// ClientInfo contains information about the client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the result from initialize.
type InitializeResult struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]interface{} `json:"capabilities"`
	ServerInfo      ServerInfo             `json:"serverInfo"`
}

// ServerInfo contains information about the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCPResultMap tracks result counts by status.
type MCPResultMap map[string]int64

// MCPClient handles MCP protocol interactions.
type MCPClient struct {
	httpClient      *http.Client
	url             string
	tool            string
	args            map[string]interface{}
	sessions        int
	sessionIDs      []string
	protocolVersion string // from server initialize result, sent on subsequent requests
	currentSession  atomic.Int32
	reqTimeout      time.Duration
	requestIDBase   int64
	requestIDOffset atomic.Int64
}

// MCPOptions contains options for MCP testing.
type MCPOptions struct {
	URL        string
	Tool       string
	Args       string // JSON string
	Sessions   int
	ReqTimeout time.Duration
}

// RunnerOptions includes the base RunnerOptions plus MCP specific options.
type RunnerOptions struct {
	periodic.RunnerOptions
	MCPOptions
}

// RunnerResults is the aggregated result of an MCP Runner.
type RunnerResults struct {
	periodic.RunnerResults
	MCPOptions
	RetCodes    MCPResultMap
	SocketCount int
	client      *MCPClient
	aborter     *periodic.Aborter
}

// NewMCPClient creates and initializes a new MCP client.
func NewMCPClient(o *MCPOptions) (*MCPClient, error) {
	c := &MCPClient{
		httpClient: &http.Client{
			Timeout: o.ReqTimeout,
		},
		url:        o.URL,
		tool:       o.Tool,
		sessions:   o.Sessions,
		reqTimeout: o.ReqTimeout,
	}

	if o.ReqTimeout == 0 {
		log.Debugf("Request timeout not set, using default %v", fhttp.HTTPReqTimeOutDefaultValue)
		c.reqTimeout = fhttp.HTTPReqTimeOutDefaultValue
		c.httpClient.Timeout = c.reqTimeout
	}

	// Parse args JSON
	if o.Args != "" {
		if err := json.Unmarshal([]byte(o.Args), &c.args); err != nil {
			return nil, fmt.Errorf("failed to parse args JSON: %w", err)
		}
	} else {
		c.args = make(map[string]interface{})
	}

	return c, nil
}

// Initialize performs the MCP initialization handshake for all sessions.
func (c *MCPClient) Initialize(connID int) error {
	c.sessionIDs = make([]string, c.sessions)
	c.requestIDBase = int64(connID) * 1000000 // offset by connection ID to avoid conflicts

	for i := 0; i < c.sessions; i++ {
		// Send initialize request
		initParams := InitializeParams{
			ProtocolVersion: "2024-11-05",
			Capabilities: map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			ClientInfo: ClientInfo{
				Name:    "fortio-mcp-client",
				Version: "1.0",
			},
		}

		initReq := JSONRPCRequest{
			JSONRPC: "2.0",
			Method:  "initialize",
			Params:  initParams,
			ID:      c.nextRequestID(),
		}

		initResp, respHeaders, err := c.sendRequest(&initReq, nil)
		if err != nil {
			return fmt.Errorf("initialize request failed for session %d: %w", i, err)
		}

		if initResp.Error != nil {
			return fmt.Errorf("initialize error for session %d: %s", i, initResp.Error.Message)
		}

		// Parse initialize response to get session info if provided
		var initResult InitializeResult
		if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
			return fmt.Errorf("failed to parse initialize response for session %d: %w", i, err)
		}
		c.protocolVersion = initResult.ProtocolVersion

		// Store session identifier from server or fallback to local id
		if sid := respHeaders.Get("mcp-session-id"); sid != "" {
			c.sessionIDs[i] = sid
		} else {
			c.sessionIDs[i] = fmt.Sprintf("session-%d-%d", connID, i)
		}

		// Send initialized notification with MCP session headers
		initializedNotif := JSONRPCRequest{
			JSONRPC: "2.0",
			Method:  "notifications/initialized",
			Params:  map[string]interface{}{},
		}
		notifHeaders := map[string]string{
			"mcp-protocol-version": initResult.ProtocolVersion,
		}
		if sid := respHeaders.Get("mcp-session-id"); sid != "" {
			notifHeaders["mcp-session-id"] = sid
		}
		if err := c.sendNotification(&initializedNotif, notifHeaders); err != nil {
			return fmt.Errorf("initialized notification failed for session %d: %w", i, err)
		}

		log.Debugf("Initialized session %d: %s", i, c.sessionIDs[i])
	}

	return nil
}

// nextRequestID generates a unique request ID.
func (c *MCPClient) nextRequestID() int64 {
	return c.requestIDBase + c.requestIDOffset.Add(1)
}

// parseSSEResponse reads a text/event-stream body and returns the first JSON-RPC response from a "data:" line.
func parseSSEResponse(r io.Reader) (*JSONRPCResponse, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			jsonStr := strings.TrimPrefix(line, "data: ")
			jsonStr = strings.TrimSpace(jsonStr)
			if jsonStr == "" {
				continue
			}
			var resp JSONRPCResponse
			if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal SSE data line: %w", err)
			}
			return &resp, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read SSE stream: %w", err)
	}
	return nil, fmt.Errorf("no data line in SSE response")
}

// sendRequest sends a JSON-RPC request and returns the response and response headers.
// extraHeaders are optional (e.g. mcp-session-id, mcp-protocol-version).
func (c *MCPClient) sendRequest(req *JSONRPCRequest, extraHeaders map[string]string) (*JSONRPCResponse, http.Header, error) {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range extraHeaders {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(httpResp.Body)
		return nil, nil, fmt.Errorf("HTTP error %d: %s", httpResp.StatusCode, string(body))
	}

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response body: %w", err)
	}

	contentType := httpResp.Header.Get("Content-Type")
	var resp *JSONRPCResponse
	if strings.Contains(contentType, "text/event-stream") {
		resp, err = parseSSEResponse(bytes.NewReader(respBody))
		if err != nil {
			return nil, nil, err
		}
	} else {
		var r JSONRPCResponse
		if err := json.Unmarshal(respBody, &r); err != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal response: %w", err)
		}
		resp = &r
	}
	return resp, httpResp.Header.Clone(), nil
}

// sendNotification sends a JSON-RPC notification (no response expected).
// extraHeaders are optional MCP headers (e.g. mcp-session-id, mcp-protocol-version).
func (c *MCPClient) sendNotification(notif *JSONRPCRequest, extraHeaders map[string]string) error {
	reqBody, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range extraHeaders {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer httpResp.Body.Close()

	// For notifications, accept 200 OK, 202 Accepted, 204 No Content
	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted && httpResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("HTTP error %d: %s", httpResp.StatusCode, string(body))
	}

	// Drain body (required for SSE streams and connection reuse)
	_, _ = io.Copy(io.Discard, httpResp.Body)
	return nil
}

// CallTool performs a tool call using one of the initialized sessions.
func (c *MCPClient) CallTool() error {
	// Round-robin through sessions (thread-safe)
	sessionIdx := int(c.currentSession.Add(1)-1) % c.sessions

	// MCP session headers required for tool calls
	headers := make(map[string]string)
	if c.protocolVersion != "" {
		headers["mcp-protocol-version"] = c.protocolVersion
	}
	if sessionIdx < len(c.sessionIDs) && c.sessionIDs[sessionIdx] != "" {
		headers["mcp-session-id"] = c.sessionIDs[sessionIdx]
	}

	// Prepare tool call request
	toolParams := map[string]interface{}{
		"name":      c.tool,
		"arguments": c.args,
	}

	toolReq := JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  toolParams,
		ID:      c.nextRequestID(),
	}

	resp, _, err := c.sendRequest(&toolReq, headers)
	if err != nil {
		return err
	}

	if resp.Error != nil {
		return fmt.Errorf("tool call error: %s", resp.Error.Message)
	}

	log.Debugf("Tool call succeeded on session %d", sessionIdx)
	return nil
}

// Close closes the MCP client.
func (c *MCPClient) Close() {
	// For HTTP-based MCP, there's no persistent connection to close
	// But we could send shutdown notifications here if needed
}

// Run tests MCP request fetching. Main call being run at the target QPS.
func (mcpstate *RunnerResults) Run(_ context.Context, t periodic.ThreadID) (bool, string) {
	log.Debugf("Calling in %d", t)
	err := mcpstate.client.CallTool()
	if err != nil {
		errStr := err.Error()
		mcpstate.RetCodes[errStr]++
		return false, errStr
	}
	mcpstate.RetCodes["OK"]++
	return true, "OK"
}

// RunMCPTest runs an MCP test and returns the aggregated stats.
func RunMCPTest(o *RunnerOptions) (*RunnerResults, error) {
	o.RunType = "MCP"
	log.Infof("Starting MCP test for %s with %d threads, %d sessions per thread at %.1f qps",
		o.URL, o.NumThreads, o.Sessions, o.QPS)

	r := periodic.NewPeriodicRunner(&o.RunnerOptions)
	defer r.Options().Abort()
	numThreads := r.Options().NumThreads
	out := r.Options().Out

	total := RunnerResults{
		aborter:  r.Options().Stop,
		RetCodes: make(MCPResultMap),
	}
	total.URL = o.URL
	total.Tool = o.Tool
	total.Args = o.Args
	total.Sessions = o.Sessions

	mcpstate := make([]RunnerResults, numThreads)
	var err error

	// Create clients and initialize sessions
	for i := range numThreads {
		r.Options().Runners[i] = &mcpstate[i]

		mcpstate[i].client, err = NewMCPClient(&o.MCPOptions)
		if mcpstate[i].client == nil {
			return nil, fmt.Errorf("unable to create client %d for %s: %w", i, o.URL, err)
		}

		// Warmup: Initialize sessions
		if o.Exactly <= 0 {
			err = mcpstate[i].client.Initialize(i)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize client %d: %w", i, err)
			}
			if i == 0 {
				log.LogVf("Initialized %d sessions for connection 0", o.Sessions)
			}
		}

		mcpstate[i].aborter = total.aborter
		mcpstate[i].RetCodes = make(MCPResultMap)
	}

	// Run the load test
	total.RunnerResults = r.Run()

	// Aggregate results
	keys := []string{}
	for i := range numThreads {
		mcpstate[i].client.Close()
		for k := range mcpstate[i].RetCodes {
			if _, exists := total.RetCodes[k]; !exists {
				keys = append(keys, k)
			}
			total.RetCodes[k] += mcpstate[i].RetCodes[k]
		}
	}
	total.SocketCount = numThreads // one per thread

	// Cleanup
	r.Options().ReleaseRunners()

	// Print results
	totalCount := float64(total.DurationHistogram.Count)
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(out, "mcp %s : %d (%.1f %%)\n", k, total.RetCodes[k],
			100.*float64(total.RetCodes[k])/totalCount)
	}

	return &total, nil
}
