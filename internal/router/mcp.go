package router

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
	"boxy.dev/boxy/internal/api"
	ctrlclient "boxy.dev/boxy/internal/controller"
)

type bashParams struct {
	Command        string `json:"command" jsonschema:"Shell command to execute,required"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty" jsonschema:"Timeout in seconds (default 60)"`
}

type readFileParams struct {
	Path string `json:"path" jsonschema:"Path under /workspace (absolute or relative),required"`
}

type writeFileParams struct {
	Path     string `json:"path" jsonschema:"Absolute path inside the sandbox (e.g. /workspace/file),required"`
	Content  string `json:"content" jsonschema:"File content,required"`
	Encoding string `json:"encoding,omitempty" jsonschema:"Content encoding: utf-8 (default) or base64 for binary"`
}

type editFileParams struct {
	Path       string `json:"path" jsonschema:"Absolute path inside the sandbox,required"`
	OldString  string `json:"oldString" jsonschema:"Exact text to replace,required"`
	NewString  string `json:"newString" jsonschema:"Replacement text,required"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"Replace all occurrences (default false)"`
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
			return s.mcpBashTool(ctx, req, sandboxID, sessionID, params)
		})

		mcp.AddTool(mcpSrv, &mcp.Tool{
			Name:        "read_file",
			Description: "Read a file from the sandbox /workspace directory",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, params readFileParams) (*mcp.CallToolResult, any, error) {
			return s.mcpReadFileTool(ctx, sandboxID, sessionID, params)
		})

		mcp.AddTool(mcpSrv, &mcp.Tool{
			Name:        "write_file",
			Description: "Create or overwrite a file in the sandbox /workspace directory",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, params writeFileParams) (*mcp.CallToolResult, any, error) {
			return s.mcpWriteFileTool(ctx, sandboxID, sessionID, params)
		})

		mcp.AddTool(mcpSrv, &mcp.Tool{
			Name:        "edit_file",
			Description: "Replace an exact string in a file in the sandbox /workspace directory",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, params editFileParams) (*mcp.CallToolResult, any, error) {
			return s.mcpEditFileTool(ctx, sandboxID, sessionID, params)
		})

		return mcpSrv
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

func toolErrResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: true,
	}
}

func toolError(text string) (*mcp.CallToolResult, any, error) {
	return toolErrResult(text), nil, nil
}

func (s *Server) resolveToolSession(ctx context.Context, sandboxID, sessionID string) (*boxyv1.Session, *mcp.CallToolResult) {
	var session *boxyv1.Session
	var err error

	if strings.TrimSpace(sessionID) != "" {
		session, err = s.lookupSession(ctx, sessionID)
		if err != nil {
			return nil, toolErrResult("store error: " + err.Error())
		}
		if session == nil {
			return nil, toolErrResult(fmt.Sprintf("session %q not found", sessionID))
		}
		if strings.TrimSpace(sandboxID) != "" && session.Spec.SandboxID != sandboxID {
			return nil, toolErrResult(fmt.Sprintf("session %q does not belong to sandbox %q", sessionID, sandboxID))
		}
	}

	if session == nil {
		_, resolvedSessionID, resolveErr := s.resolveDefaultSession(ctx, sandboxID)
		if resolveErr != nil {
			return nil, toolErrResult("no session specified and default session unavailable: " + resolveErr.Error())
		}
		session, err = s.lookupSession(ctx, resolvedSessionID)
		if err != nil || session == nil {
			return nil, toolErrResult(fmt.Sprintf("default session %q not found", resolvedSessionID))
		}
	}

	if err := s.canResourceAccess(ctx, "update", "sessions", session.Name); err != nil {
		return nil, toolErrResult("forbidden")
	}
	if session.Status.Phase != boxyv1.SandboxPhaseRunning {
		return nil, toolErrResult(fmt.Sprintf("session %q not running (phase: %s)", session.Spec.SessionID, session.Status.Phase))
	}
	return session, nil
}

func toolText(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

func (s *Server) mcpBashTool(ctx context.Context, req *mcp.CallToolRequest, sandboxID, sessionID string, params bashParams) (*mcp.CallToolResult, any, error) {
	if params.Command == "" {
		return toolError("command is required")
	}
	if params.TimeoutSeconds <= 0 {
		params.TimeoutSeconds = 60
	}

	session, errRes := s.resolveToolSession(ctx, sandboxID, sessionID)
	if errRes != nil {
		return errRes, nil, nil
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
	execReq := ctrlclient.ExecReq{
		SandboxID:      session.Spec.SessionID,
		Command:        "sh",
		Args:           []string{"-c", params.Command},
		TimeoutSeconds: params.TimeoutSeconds,
	}

	var stdout, stderr string
	var exitCode int
	var timedOut bool

	progressToken := req.Params.GetProgressToken()
	if progressToken != nil && req.Session != nil {
		var stdoutBuf, stderrBuf strings.Builder
		chunkIdx := float64(0)
		streamResult, streamErr := s.ctrlClient.ExecStream(ctx, baseURL, execReq, func(evtType, data string) {
			switch evtType {
			case "stdout":
				stdoutBuf.WriteString(data)
			case "stderr":
				stderrBuf.WriteString(data)
			default:
				return
			}
			chunkIdx++
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: progressToken,
				Progress:      chunkIdx,
				Message:       data,
			})
		})
		if streamErr != nil {
			return toolError("exec error: " + streamErr.Error())
		}
		stdout = stdoutBuf.String()
		stderr = stderrBuf.String()
		exitCode = streamResult.ExitCode
		timedOut = streamResult.TimedOut
	} else {
		syncResult, syncErr := s.ctrlClient.Exec(ctx, baseURL, execReq)
		if syncErr != nil {
			return toolError("exec error: " + syncErr.Error())
		}
		stdout = syncResult.Stdout
		stderr = syncResult.Stderr
		exitCode = syncResult.ExitCode
		timedOut = syncResult.TimedOut
	}

	go s.touchLastExecSession(session.DeepCopy())

	text := stdout
	if stderr != "" {
		if text != "" {
			text += "\n"
		}
		text += stderr
	}
	if exitCode != 0 {
		text += fmt.Sprintf("\n[exit code: %d]", exitCode)
	}
	if timedOut {
		text += "\n[timed out]"
	}

	if exitCode != 0 {
		return toolError(text)
	}
	return toolText(text)
}

func (s *Server) mcpReadFileTool(ctx context.Context, sandboxID, sessionID string, params readFileParams) (*mcp.CallToolResult, any, error) {
	if err := api.ValidateFilePath(params.Path); err != nil {
		return toolError(err.Error())
	}

	session, errRes := s.resolveToolSession(ctx, sandboxID, sessionID)
	if errRes != nil {
		return errRes, nil, nil
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return toolError("concurrency limit reached, try again later")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := s.ctrlClient.ReadFile(ctx, s.controllerURLFromSession(session), ctrlclient.FileReadReq{
		SandboxID: session.Spec.SessionID,
		Path:      params.Path,
	})
	if err != nil {
		return toolError("read error: " + err.Error())
	}

	go s.touchLastExecSession(session.DeepCopy())
	return toolText(res.Content)
}

func (s *Server) mcpWriteFileTool(ctx context.Context, sandboxID, sessionID string, params writeFileParams) (*mcp.CallToolResult, any, error) {
	if err := api.ValidateFilePath(params.Path); err != nil {
		return toolError(err.Error())
	}
	if err := api.ValidateFileEncoding(params.Encoding); err != nil {
		return toolError(err.Error())
	}

	session, errRes := s.resolveToolSession(ctx, sandboxID, sessionID)
	if errRes != nil {
		return errRes, nil, nil
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return toolError("concurrency limit reached, try again later")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := s.ctrlClient.WriteFile(ctx, s.controllerURLFromSession(session), ctrlclient.FileWriteReq{
		SandboxID: session.Spec.SessionID,
		Path:      params.Path,
		Content:   params.Content,
		Encoding:  params.Encoding,
	})
	if err != nil {
		return toolError("write error: " + err.Error())
	}

	go s.touchLastExecSession(session.DeepCopy())
	return toolText(fmt.Sprintf("wrote %d bytes to %s", res.BytesWritten, res.Path))
}

func (s *Server) mcpEditFileTool(ctx context.Context, sandboxID, sessionID string, params editFileParams) (*mcp.CallToolResult, any, error) {
	if err := api.ValidateFilePath(params.Path); err != nil {
		return toolError(err.Error())
	}
	if params.OldString == "" {
		return toolError("oldString is required")
	}

	session, errRes := s.resolveToolSession(ctx, sandboxID, sessionID)
	if errRes != nil {
		return errRes, nil, nil
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return toolError("concurrency limit reached, try again later")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := s.ctrlClient.EditFile(ctx, s.controllerURLFromSession(session), ctrlclient.FileEditReq{
		SandboxID:  session.Spec.SessionID,
		Path:       params.Path,
		OldString:  params.OldString,
		NewString:  params.NewString,
		ReplaceAll: params.ReplaceAll,
	})
	if err != nil {
		return toolError("edit error: " + err.Error())
	}

	go s.touchLastExecSession(session.DeepCopy())
	return toolText(fmt.Sprintf("made %d replacement(s) in %s", res.Replacements, res.Path))
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
	if err := s.canResourceAccess(ctx, "get", "sandboxes", sandboxID); err != nil {
		return "", "", fmt.Errorf("forbidden")
	}
	if sb == nil {
		if _, err := s.createSandboxFromBody(ctx, s.cfg.DefaultSandboxConfig); err != nil {
			return "", "", fmt.Errorf("create default sandbox config: %w", err)
		}
	}

	prefix := sandboxID
	if len(prefix) > 55 {
		prefix = prefix[:55]
	}
	defaultSessionID := prefix + "-session"

	sess, err := s.lookupSession(ctx, defaultSessionID)
	if err != nil {
		return "", "", err
	}
	if sess == nil || sess.Status.Phase == boxyv1.SandboxPhaseTerminated {
		if err := s.canResourceAccess(ctx, "create", "sessions", defaultSessionID); err != nil {
			return "", "", fmt.Errorf("forbidden")
		}
		if sess != nil {
			if err := s.k8sClient.Delete(ctx, sess); err != nil {
				return "", "", fmt.Errorf("delete terminated session: %w", err)
			}
		}
		createCtx, cancel := context.WithTimeout(ctx, s.cfg.CreateTimeout+5*time.Second)
		defer cancel()
		if _, err := s.createAndWaitForSession(createCtx, defaultSessionID, sandboxID, "system"); err != nil {
			return "", "", fmt.Errorf("create default session: %w", err)
		}
	}

	return sandboxID, defaultSessionID, nil
}
