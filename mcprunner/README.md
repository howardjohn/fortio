# MCP Load Testing

Fortio now supports load testing for Model Context Protocol (MCP) servers using the `mcp://` URL prefix.

## Overview

MCP (Model Context Protocol) is a JSON-RPC based protocol for communication between AI applications and context providers. Fortio's MCP load testing feature allows you to:

- Test MCP server performance under load
- Initialize multiple sessions per connection
- Call MCP tools repeatedly with configurable arguments
- Measure latency and throughput statistics

## Usage

### Basic Example

```bash
fortio load -qps 10 -t 10s -c 2 \
  --mcp-tool echo \
  --mcp-args '{"message":"hello"}' \
  mcp://localhost:8889/
```

This command:
- Runs at 10 queries per second for 10 seconds
- Uses 2 concurrent connections
- Calls the `echo` tool with the specified arguments
- Connects to the MCP server at `localhost:8889`

### Flags

#### Required Flags
- `--mcp-tool <name>` - The name of the MCP tool to call (required for MCP load testing)

#### Optional Flags
- `--mcp-sessions <n>` - Number of MCP sessions per connection (default: 1)
- `--mcp-args <json>` - JSON string containing tool arguments (default: `{}`)

### Standard Load Testing Flags

All standard Fortio load testing flags are supported:
- `-qps <n>` - Queries per second (0 for max QPS)
- `-t <duration>` - Test duration (e.g., `10s`, `2m`)
- `-c <n>` - Number of concurrent connections/threads
- `-n <n>` - Exact number of calls to make (instead of duration)
- `-p <percentiles>` - Percentiles to report (default: `50,75,90,99,99.9`)
- `-json <file>` - Save results to JSON file

## Examples

### Simple Tool Call
```bash
# Test a simple tool with 5 QPS for 30 seconds
fortio load -qps 5 -t 30s \
  --mcp-tool calculator \
  --mcp-args '{"operation":"add","a":5,"b":3}' \
  mcp://localhost:8889/
```

### Multiple Sessions
```bash
# Test with 3 sessions per connection
fortio load -qps 20 -t 1m -c 4 \
  --mcp-sessions 3 \
  --mcp-tool search \
  --mcp-args '{"query":"test"}' \
  mcp://localhost:8889/
```

### High Load Test
```bash
# Maximum QPS test with 10 connections
fortio load -qps 0 -t 10s -c 10 \
  --mcp-tool status \
  mcp://localhost:8889/
```

### Exact Call Count
```bash
# Make exactly 100 calls
fortio load -n 100 -c 1 \
  --mcp-tool ping \
  mcp://localhost:8889/
```

## Protocol Details

### Initialization Flow
During the warmup phase, Fortio performs the MCP initialization handshake for each session:

1. Sends an `initialize` request with protocol version and capabilities
2. Receives the server's capabilities and info
3. Sends an `initialized` notification

This happens once per session during warmup, not during the load test itself.

### Tool Calls
The main load test repeatedly calls the specified tool using the `tools/call` method:

```json
{
  "jsonrpc": "2.0",
  "method": "tools/call",
  "params": {
    "name": "tool_name",
    "arguments": { /* your args */ }
  },
  "id": <unique_request_id>
}
```

Each request gets a unique JSON-RPC request ID to avoid conflicts.

### Session Management
- Sessions are created per connection (thread)
- The `--mcp-sessions` flag controls how many sessions are initialized per connection
- Tool calls round-robin across sessions within each connection

## Output

The output includes standard Fortio metrics plus MCP-specific information:

```
Fortio 0.0.0 running at 10 queries per second, 4->4 procs, for 10s: mcp://localhost:8889/
Starting MCP test for http://localhost:8889/ with 2 threads, 1 sessions per thread at 10.0 qps
...
mcp OK : 100 (100.0 %)
All done 100 calls (plus 2 warmup) 1.265 ms avg, 10.0 qps
```

## Notes

- Only HTTP transport is supported (not stdio or other transports)
- The URL prefix `mcp://` is automatically converted to `http://`
- For HTTPS, use standard Fortio TLS flags (`-cacert`, `-cert`, `-key`, etc.)
- Session initialization occurs during warmup and is not included in latency measurements
- Tool arguments must be valid JSON

## Troubleshooting

### Tool not found error
Make sure the `--mcp-tool` flag matches an available tool on your MCP server.

### JSON parsing errors
Verify that `--mcp-args` contains valid JSON. Remember to properly escape quotes in your shell:
```bash
--mcp-args '{"key":"value"}'  # Good
--mcp-args "{\"key\":\"value\"}"  # Also works
```

### Connection refused
Ensure your MCP server is running and listening on the specified port.

### High latency
Check if your MCP server can handle the requested QPS. Try reducing `-qps` or increasing `-c` for more connections.
