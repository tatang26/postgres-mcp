package mcpserver

import (
	"encoding/json/jsontext"
)

// protocolVersion is the MCP protocol version this server implements and
// advertises during initialize.
const protocolVersion = "2024-11-05"

// JSON-RPC 2.0 standard error codes (see https://www.jsonrpc.org/specification).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// request is the wire representation of an incoming JSON-RPC message. A
// notification (no response expected) is any request with a nil ID.
type request struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  jsontext.Value `json:"params,omitempty"`
}

// response is the wire representation of an outgoing JSON-RPC message.
type response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id,omitzero"`
	Result  any            `json:"result,omitempty"`
	Error   *rpcError      `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func newResultResponse(id jsontext.Value, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func newErrorResponse(id jsontext.Value, code int, message string) response {
	if len(id) == 0 {
		// JSON-RPC requires the "id" member to be present (as null) on an
		// error response when the request's id could not be determined
		// (e.g. a parse error), rather than omitted entirely.
		id = jsontext.Value("null")
	}
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

// initializeParams is the subset of the client's initialize request we care
// about. Unknown/extra fields are ignored.
type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
}

// toolDescriptor is the wire representation of a tool in a tools/list
// response.
type toolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments jsontext.Value `json:"arguments"`
}

// ContentBlock is a single piece of tool output content. Only the "text"
// type is produced by this server.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult is the result of invoking a tool, as returned in a
// tools/call response. IsError signals a business-logic failure (e.g. a
// SQL error, or a restricted-mode violation) as opposed to a JSON-RPC
// protocol-level error.
type CallToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitzero"`
}

// TextResult builds a successful CallToolResult with a single text block.
func TextResult(text string) *CallToolResult {
	return &CallToolResult{Content: []ContentBlock{{Type: "text", Text: text}}}
}

// ErrorResult builds a failed CallToolResult with a single text block
// describing err. Use this for business-logic failures that the caller
// (the LLM) should be able to see and react to, as opposed to returning a
// Go error from a Handler, which surfaces as a JSON-RPC protocol error.
func ErrorResult(err error) *CallToolResult {
	return &CallToolResult{
		Content: []ContentBlock{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}
