package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// runLines feeds each of lines (already-marshaled JSON-RPC messages) to a
// fresh Run over an in-memory pipe and returns the decoded response
// objects, in order, that the server wrote back.
func runLines(t *testing.T, srv *Server, lines []string) []map[string]any {
	t.Helper()

	input := strings.Join(lines, "\n") + "\n"
	var out bytes.Buffer

	if err := srv.Run(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var responses []map[string]any
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("failed to decode response line %q: %v", l, err)
		}
		responses = append(responses, m)
	}
	return responses
}

func TestInitialize(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
	})

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	result, ok := responses[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %v", responses[0])
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("expected serverInfo object, got %v", result)
	}
	if serverInfo["name"] != "postgres-mcp" || serverInfo["version"] != "0.1.0" {
		t.Errorf("unexpected serverInfo: %v", serverInfo)
	}
}

func TestNotificationProducesNoResponse(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	})

	// Only the initialize and ping requests have IDs, so only 2 responses
	// should be emitted; the notification must be silently swallowed.
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses (notification should produce none), got %d: %v", len(responses), responses)
	}
	if responses[0]["id"] != float64(1) {
		t.Errorf("expected first response id 1, got %v", responses[0]["id"])
	}
	if responses[1]["id"] != float64(2) {
		t.Errorf("expected second response id 2, got %v", responses[1]["id"])
	}
}

func TestToolsList(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")
	srv.AddTool(&Tool{
		Name:        "echo",
		Description: "echoes input",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
			return TextResult("ok"), nil
		},
	})
	srv.AddTool(&Tool{
		Name:        "noop",
		Description: "does nothing",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
			return TextResult("ok"), nil
		},
	})

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	})

	result := responses[0]["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] != "echo" {
		t.Errorf("expected tools in registration order, first was %v", first["name"])
	}
}

func TestToolsCallSuccess(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")
	srv.AddTool(&Tool{
		Name: "echo",
		Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
			var params struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return nil, err
			}
			return TextResult("you said: " + params.Text), nil
		},
	})

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
	})

	result := responses[0]["result"].(map[string]any)
	if result["isError"] != nil {
		t.Errorf("expected no isError field, got %v", result["isError"])
	}
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "you said: hi" {
		t.Errorf("unexpected content text: %v", content["text"])
	}
}

func TestToolsCallBusinessError(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")
	srv.AddTool(&Tool{
		Name: "fail",
		Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
			return ErrorResult(errors.New("boom")), nil
		},
	})

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fail","arguments":{}}}`,
	})

	// Business-logic errors are reported as a successful JSON-RPC response
	// with isError:true, not a JSON-RPC error, so the LLM can see the
	// message.
	if responses[0]["error"] != nil {
		t.Fatalf("expected no top-level JSON-RPC error, got %v", responses[0]["error"])
	}
	result := responses[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError true, got %v", result["isError"])
	}
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "boom" {
		t.Errorf("unexpected content text: %v", content["text"])
	}
}

func TestToolsCallHandlerPanicIsRecovered(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")
	srv.AddTool(&Tool{
		Name: "panics",
		Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
			panic("boom")
		},
	})
	srv.AddTool(&Tool{
		Name: "echo",
		Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
			return TextResult("still alive"), nil
		},
	})

	// A panicking handler must produce a JSON-RPC error response for that
	// call, but must not prevent the server from handling subsequent
	// calls in the same Run.
	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"panics","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
	})

	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}
	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error for panicking handler, got %v", responses[0])
	}
	if !strings.Contains(errObj["message"].(string), "panicked") {
		t.Errorf("expected panic message, got: %v", errObj["message"])
	}

	result := responses[1]["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "still alive" {
		t.Errorf("expected server to keep serving after a panic, got: %v", content["text"])
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`,
	})

	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error for unknown tool, got %v", responses[0])
	}
	if errObj["code"] != float64(codeMethodNotFound) {
		t.Errorf("expected method-not-found code, got %v", errObj["code"])
	}
}

func TestToolsCallHandlerProtocolError(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")
	srv.AddTool(&Tool{
		Name: "broken",
		Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
			return nil, errors.New("invalid arguments")
		},
	})

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"broken","arguments":{}}}`,
	})

	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error, got %v", responses[0])
	}
	if errObj["code"] != float64(codeInternalError) {
		t.Errorf("expected internal-error code, got %v", errObj["code"])
	}
}

func TestUnknownMethodWithID(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"does/not/exist"}`,
	})

	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error, got %v", responses[0])
	}
	if errObj["code"] != float64(codeMethodNotFound) {
		t.Errorf("expected method-not-found code, got %v", errObj["code"])
	}
}

func TestUnknownNotificationIsIgnored(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","method":"notifications/something_unknown"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	})

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d: %v", len(responses), responses)
	}
}

func TestMalformedJSONProducesParseError(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		`{not valid json`,
	})

	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC parse error, got %v", responses[0])
	}
	if errObj["code"] != float64(codeParseError) {
		t.Errorf("expected parse-error code, got %v", errObj["code"])
	}
}

func TestBlankLinesAreSkipped(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		``,
		`   `,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	})

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d: %v", len(responses), responses)
	}
}

func TestAddToolOverwriteKeepsStableOrder(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")
	srv.AddTool(&Tool{Name: "a", Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
		return TextResult("first"), nil
	}})
	srv.AddTool(&Tool{Name: "b", Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
		return TextResult("b"), nil
	}})
	srv.AddTool(&Tool{Name: "a", Handler: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
		return TextResult("second"), nil
	}})

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	})

	result := responses[0]["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 distinct tool names after overwrite, got %d", len(tools))
	}
	if tools[0].(map[string]any)["name"] != "a" {
		t.Errorf("expected overwritten tool to keep its original position")
	}
}

func TestToolsCallInvalidParams(t *testing.T) {
	srv := NewServer("postgres-mcp", "0.1.0")

	responses := runLines(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"not-an-object"}`,
	})

	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error, got %v", responses[0])
	}
	if errObj["code"] != float64(codeInvalidParams) {
		t.Errorf("expected invalid-params code, got %v", errObj["code"])
	}
}

func TestTextResultAndErrorResultHelpers(t *testing.T) {
	tr := TextResult("hello")
	if tr.IsError {
		t.Error("TextResult should not set IsError")
	}
	if len(tr.Content) != 1 || tr.Content[0].Text != "hello" || tr.Content[0].Type != "text" {
		t.Errorf("unexpected TextResult content: %+v", tr.Content)
	}

	er := ErrorResult(errors.New("bad"))
	if !er.IsError {
		t.Error("ErrorResult should set IsError")
	}
	if len(er.Content) != 1 || er.Content[0].Text != "bad" {
		t.Errorf("unexpected ErrorResult content: %+v", er.Content)
	}
}
