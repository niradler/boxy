package router

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
	ctrlclient "boxy.dev/boxy/internal/controller"
)

type bashParams struct {
	Command        string `json:"command" jsonschema:"Shell command to execute,required"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty" jsonschema:"Timeout in seconds (default 60)"`
}

func (s *Server) newMCPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		sandboxID := r.Header.Get("X-Sandbox-Id")

		mcpSrv := mcp.NewServer(&mcp.Implementation{
			Name:    "boxy",
			Version: "0.1.0",
		}, nil)

		mcp.AddTool(mcpSrv, &mcp.Tool{
			Name:        "bash",
			Description: "Execute a shell command in a sandbox",
		}, func(ctx context.Context, req *mcp.CallToolRequest, params bashParams) (*mcp.CallToolResult, any, error) {
			return s.mcpBashTool(ctx, sandboxID, params)
		})

		return mcpSrv
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

func toolError(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: true,
	}, nil, nil
}

func toolText(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

func (s *Server) mcpBashTool(ctx context.Context, sandboxID string, params bashParams) (*mcp.CallToolResult, any, error) {
	if params.Command == "" {
		return toolError("command is required")
	}
	if params.TimeoutSeconds <= 0 {
		params.TimeoutSeconds = 60
	}

	if sandboxID == "" {
		var err error
		sandboxID, err = s.resolveDefaultSandboxID(ctx)
		if err != nil {
			return toolError("no sandbox specified and default sandbox is not available: " + err.Error())
		}
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return toolError("concurrency limit reached, try again later")
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutSeconds)*time.Second+5*time.Second)
	defer cancel()

	sandbox, err := s.lookupSandbox(ctx, sandboxID)
	if err != nil {
		return toolError("store error: " + err.Error())
	}
	if sandbox == nil || sandbox.Status.Phase != boxyv1.SandboxPhaseRunning {
		return toolError(fmt.Sprintf("sandbox %q not found", sandboxID))
	}

	baseURL := s.controllerURL(sandbox)

	result, err := s.ctrlClient.Exec(ctx, baseURL, ctrlclient.ExecReq{
		SandboxID:      sandboxID,
		Command:        "sh",
		Args:           []string{"-c", params.Command},
		TimeoutSeconds: params.TimeoutSeconds,
	})
	if err != nil {
		return toolError("exec error: " + err.Error())
	}

	go s.touchLastExec(sandbox)

	text := result.Stdout
	if result.Stderr != "" {
		if text != "" {
			text += "\n"
		}
		text += result.Stderr
	}
	if result.ExitCode != 0 {
		text += fmt.Sprintf("\n[exit code: %d]", result.ExitCode)
	}
	if result.TimedOut {
		text += "\n[timed out]"
	}

	if result.ExitCode != 0 {
		return toolError(text)
	}
	return toolText(text)
}
