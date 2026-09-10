// Package mcpserver is a minimal, hand-rolled implementation of the Model
// Context Protocol (MCP) stdio transport: newline-delimited JSON-RPC 2.0
// messages over stdin/stdout. It intentionally implements only the subset
// of MCP needed to serve tools: initialize, notifications/initialized,
// tools/list, tools/call, and ping.
//
// Requests are processed sequentially, one line at a time, in the order
// they are read from the input stream. MCP permits pipelined/concurrent
// requests, but a single-threaded stdio server is simpler to reason about
// and is sufficient for this use case (interactive tool calls against a
// database, driven by a single client).
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
)

// maxLineSize bounds the size of a single JSON-RPC message. Tool results
// (e.g. large query result sets) can be sizable, so this is generous.
const maxLineSize = 64 * 1024 * 1024 // 64 MiB

// Handler implements a single tool's behavior. args is the raw "arguments"
// object from the tools/call request (nil/empty if the tool takes no
// arguments). Returning a non-nil error is treated as a protocol-level
// failure (e.g. malformed arguments); business-logic failures (e.g. a SQL
// error) should instead be returned via ErrorResult in a non-nil
// *CallToolResult with a nil error.
type Handler func(ctx context.Context, args jsontext.Value) (*CallToolResult, error)

// Tool is a single tool exposed by the server, as advertised in tools/list
// and invoked via tools/call.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     Handler
}

// Server holds the set of registered tools and serves them over the MCP
// stdio transport.
type Server struct {
	name    string
	version string
	tools   map[string]*Tool
	order   []string
}

// NewServer creates a Server that will identify itself as name/version
// during MCP initialize.
func NewServer(name, version string) *Server {
	return &Server{
		name:    name,
		version: version,
		tools:   make(map[string]*Tool),
	}
}

// AddTool registers a tool. Tools are listed (via tools/list) in the order
// they were added.
func (s *Server) AddTool(t *Tool) {
	if _, exists := s.tools[t.Name]; !exists {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = t
}

// Run reads newline-delimited JSON-RPC requests from r, dispatches them,
// and writes newline-delimited JSON-RPC responses to w. It returns when r
// is exhausted (EOF) or ctx is canceled. Malformed input lines produce a
// JSON-RPC parse-error response rather than terminating the loop.
func (s *Server) Run(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		line := scanner.Bytes()
		if len(bytesTrimSpace(line)) == 0 {
			continue
		}

		resp, hasResponse := s.handleLine(ctx, line)
		if !hasResponse {
			continue
		}

		if err := writeResponse(w, resp); err != nil {
			return fmt.Errorf("mcpserver: failed to write response: %w", err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcpserver: failed to read input: %w", err)
	}
	return nil
}

// handleLine parses and dispatches a single JSON-RPC message. It returns
// (response, true) if a response should be written, or (zero, false) for
// notifications (no ID) that produce no reply.
func (s *Server) handleLine(ctx context.Context, line []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return newErrorResponse(nil, codeParseError, "parse error: "+err.Error()), true
	}

	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req), true

	case "notifications/initialized":
		return response{}, false

	case "ping":
		return newResultResponse(req.ID, map[string]any{}), true

	case "tools/list":
		return s.handleToolsList(req), true

	case "tools/call":
		return s.handleToolsCall(ctx, req), true

	default:
		if isNotification {
			// Unknown notifications are silently ignored per JSON-RPC
			// semantics: there is no ID to reply to.
			return response{}, false
		}
		return newErrorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method), true
	}
}

func (s *Server) handleInitialize(req request) response {
	result := initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities: map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		ServerInfo: serverInfo{Name: s.name, Version: s.version},
	}
	return newResultResponse(req.ID, result)
}

func (s *Server) handleToolsList(req request) response {
	descriptors := make([]toolDescriptor, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		descriptors = append(descriptors, toolDescriptor{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return newResultResponse(req.ID, toolsListResult{Tools: descriptors})
}

func (s *Server) handleToolsCall(ctx context.Context, req request) response {
	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newErrorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}

	tool, ok := s.tools[params.Name]
	if !ok {
		return newErrorResponse(req.ID, codeMethodNotFound, "unknown tool: "+params.Name)
	}

	result, err := s.callHandler(ctx, tool, params.Arguments)
	if err != nil {
		return newErrorResponse(req.ID, codeInternalError, err.Error())
	}
	if result == nil {
		return newErrorResponse(req.ID, codeInternalError, "tool returned no result")
	}

	return newResultResponse(req.ID, result)
}

// callHandler invokes tool.Handler, recovering from any panic and
// converting it into an error instead of letting it escape and crash the
// whole server. A single bad row scan or nil dereference inside one
// tool's handler shouldn't take down every other in-flight and future
// tool call in the same process.
func (s *Server) callHandler(ctx context.Context, tool *Tool, args jsontext.Value) (result *CallToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool %q panicked: %v", tool.Name, r)
		}
	}()

	return tool.Handler(ctx, args)
}

func writeResponse(w io.Writer, resp response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func bytesTrimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpaceByte(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
