package router

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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
		sessionID := r.Header.Get("X-Session-Id")

		mcpSrv := mcp.NewServer(&mcp.Implementation{
			Name:    "boxy",
			Version: "0.1.0",
		}, nil)

		mcp.AddTool(mcpSrv, &mcp.Tool{
			Name:        "bash",
			Description: "Execute a shell command in a sandbox",
		}, func(ctx context.Context, req *mcp.CallToolRequest, params bashParams) (*mcp.CallToolResult, any, error) {
			return s.mcpBashTool(ctx, sandboxID, sessionID, params)
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

func (s *Server) mcpBashTool(ctx context.Context, sandboxID, sessionID string, params bashParams) (*mcp.CallToolResult, any, error) {
	if params.Command == "" {
		return toolError("command is required")
	}
	if params.TimeoutSeconds <= 0 {
		params.TimeoutSeconds = 60
	}

	var session *boxyv1.Session
	var err error

	if strings.TrimSpace(sessionID) != "" {
		session, err = s.lookupSession(ctx, sessionID)
		if err != nil {
			return toolError("store error: " + err.Error())
		}
	}

	if session == nil {
		resolvedSandboxID, resolvedSessionID, resolveErr := s.resolveDefaultSession(ctx, sandboxID)
		if resolveErr != nil {
			return toolError("no session specified and default session unavailable: " + resolveErr.Error())
		}
		sandboxID = resolvedSandboxID
		sessionID = resolvedSessionID
		session, err = s.lookupSession(ctx, sessionID)
		if err != nil || session == nil {
			return toolError(fmt.Sprintf("default session %q not found", sessionID))
		}
	}

	if session.Status.Phase != boxyv1.SandboxPhaseRunning {
		return toolError(fmt.Sprintf("session %q not running (phase: %s)", session.Spec.SessionID, session.Status.Phase))
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return toolError("concurrency limit reached, try again later")
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutSeconds)*time.Second+5*time.Second)
	defer cancel()

	baseURL := s.controllerURLFromSession(session)
	result, err := s.ctrlClient.Exec(ctx, baseURL, ctrlclient.ExecReq{
		SandboxID:      session.Spec.SessionID,
		Command:        "sh",
		Args:           []string{"-c", params.Command},
		TimeoutSeconds: params.TimeoutSeconds,
	})
	if err != nil {
		return toolError("exec error: " + err.Error())
	}

	go s.touchLastExecSession(session.DeepCopy())

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

func (s *Server) resolveDefaultSession(ctx context.Context, sandboxID string) (string, string, error) {
	if !s.cfg.DefaultSandboxEnabled || s.cfg.DefaultSandboxConfig == nil {
		return "", "", fmt.Errorf("default sandbox is disabled")
	}

	if sandboxID == "" {
		sandboxID = s.cfg.DefaultSandboxConfig.SandboxID
	}

	sb, err := s.lookupSandbox(ctx, sandboxID)
	if err != nil {
		return "", "", fmt.Errorf("lookup sandbox config: %w", err)
	}
	if sb == nil {
		if _, err := s.createSandboxFromBody(ctx, s.cfg.DefaultSandboxConfig); err != nil {
			return "", "", fmt.Errorf("create default sandbox config: %w", err)
		}
	}

	defaultSessionID := sandboxID + "-session"

	sess, err := s.lookupSession(ctx, defaultSessionID)
	if err != nil {
		return "", "", err
	}
	if sess == nil || sess.Status.Phase == boxyv1.SandboxPhaseTerminated {
		createCtx, cancel := context.WithTimeout(ctx, s.cfg.CreateTimeout+5*time.Second)
		defer cancel()
		if _, err := s.createAndWaitForSession(createCtx, defaultSessionID, sandboxID, "system"); err != nil {
			return "", "", fmt.Errorf("create default session: %w", err)
		}
	}

	return sandboxID, defaultSessionID, nil
}
