package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The app's agent surface.
//
// Every app on the platform exposes one, free, in every plan. Charging for
// agent access to a customer's own data is the single-sign-on tax wearing a
// new hat, and one line on a pricing page would undo the position.
//
// It is a view onto the same operations under the same identity, never a
// second API with its own idea of who may do what: this handler sits behind
// the same requireUser as every admin page, so a tool call is attributed to
// the same person a click would be.

// protocolVersion is what this server speaks when a client names none.
const protocolVersion = "2025-06-18"

// rpcRequest is one JSON-RPC 2.0 call. A request with no id is a
// notification and is answered with nothing at all.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC error codes, as the specification fixes them.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

// tool is one operation an agent may invoke.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// run executes it. Every tool here is read-only; a destructive one would
	// have to be declared as such and confirmed, because an agent in a loop at
	// three in the morning is not a person clicking a button.
	run func(ctx context.Context, a *app, args map[string]any) (any, error) `json:"-"`
}

func (a *app) tools() []tool {
	return []tool{
		{
			Name:        "list_forms",
			Description: "List every form in this installation, with its address, its audience (public: anyone with the link; internal: members only) and how many answers it has received.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			run: func(ctx context.Context, a *app, _ map[string]any) (any, error) {
				forms, err := a.store.ListForms(ctx)
				if err != nil {
					return nil, err
				}
				counts, err := a.store.CountSubmissions(ctx)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(forms))
				for _, form := range forms {
					out = append(out, map[string]any{
						"id": form.ID, "slug": form.Slug, "title": form.Title,
						"description": form.Description,
						"fields":      form.Fields, "submissions": counts[form.ID],
						"audience": form.Audience,
					})
				}
				return map[string]any{"forms": out}, nil
			},
		},
		{
			Name:        "list_submissions",
			Description: "Read the answers to one form, newest first.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"form":  map[string]any{"type": "string", "description": "The form's slug or id."},
					"limit": map[string]any{"type": "integer", "description": "How many to return, at most 1000."},
				},
				"required": []string{"form"},
			},
			run: func(ctx context.Context, a *app, args map[string]any) (any, error) {
				named, _ := args["form"].(string)
				if strings.TrimSpace(named) == "" {
					return nil, fmt.Errorf("say which form, by slug or id")
				}
				form, err := a.store.FormBySlug(ctx, named)
				if err != nil {
					if form, err = a.store.FormByID(ctx, named); err != nil {
						return nil, fmt.Errorf("no form called %q", named)
					}
				}
				limit := 200
				if raw, ok := args["limit"].(float64); ok && raw > 0 {
					limit = int(raw)
				}
				submissions, err := a.store.ListSubmissions(ctx, form.ID, limit)
				if err != nil {
					return nil, err
				}
				return map[string]any{"form": form.Slug, "submissions": submissions}, nil
			},
		},
		{
			Name:        "export_installation",
			Description: "Export every form and answer as a portable archive. The same archive this app's importer accepts.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			run: func(ctx context.Context, a *app, _ map[string]any) (any, error) {
				return a.store.Export(ctx)
			},
		},
	}
}

func (a *app) mcp(w http.ResponseWriter, r *http.Request) {
	var request rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0", Error: &rpcError{Code: codeParse, Message: err.Error()},
		})
		return
	}
	if request.JSONRPC != "2.0" {
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0", ID: request.ID,
			Error: &rpcError{Code: codeInvalidRequest, Message: "expected jsonrpc 2.0"},
		})
		return
	}

	// A notification carries no id and is answered with nothing.
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := a.dispatch(r.Context(), request)
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0", ID: request.ID, Result: result, Error: rpcErr,
	})
}

func (a *app) dispatch(ctx context.Context, request rpcRequest) (any, *rpcError) {
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = protocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forms", "version": version_()},
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		listed := a.tools()
		out := make([]tool, 0, len(listed))
		out = append(out, listed...)
		return map[string]any{"tools": out}, nil

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, &rpcError{Code: codeInvalidRequest, Message: err.Error()}
		}
		for _, candidate := range a.tools() {
			if candidate.Name != params.Name {
				continue
			}
			result, err := candidate.run(ctx, a, params.Arguments)
			if err != nil {
				// A tool that failed is a result the agent should read and act
				// on, not a transport error: isError is how MCP says so.
				return map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
				}, nil
			}
			encoded, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return nil, &rpcError{Code: codeInternal, Message: err.Error()}
			}
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(encoded)}},
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: "no tool called " + params.Name}

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unsupported method " + request.Method}
	}
}

func version_() string { return version }
