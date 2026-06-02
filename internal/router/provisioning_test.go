package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
)

// devCtx returns a context carrying the dev-token user, which authorizeResource short-circuits
// as allowed — so canResourceAccess passes in unit tests that call the resolver directly.
func devCtx() context.Context {
	return context.WithValue(context.Background(), authUserKey, &authv1.UserInfo{Username: "dev-token"})
}

// driveSessionRunning advances a session to Running once it appears, mimicking the operator so
// createAndWaitForSession can return in tests. The returned func stops the goroutine.
func driveSessionRunning(t *testing.T, srv *Server, sessionID string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Millisecond):
			}
			sess, err := srv.lookupSession(context.Background(), sessionID)
			if err != nil || sess == nil || sess.Status.Phase == boxyv1.SandboxPhaseRunning {
				continue
			}
			sess.Status.Phase = boxyv1.SandboxPhaseRunning
			sess.Status.ControllerAddress = "127.0.0.1"
			sess.Status.Port = 8080
			_ = srv.k8sClient.Status().Update(context.Background(), sess)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func errText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func countSessions(t *testing.T, srv *Server) int {
	t.Helper()
	var list boxyv1.SessionList
	if err := srv.k8sReader.List(context.Background(), &list); err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	return len(list.Items)
}

// New per-user path: X-Session-Id + X-Sandbox-Id with no existing session provisions the
// session on first contact, bound to the named config, with owner = the per-user session id.
func TestResolveToolSession_ProvisionsPerUserSessionFromConfig(t *testing.T) {
	sb := testSandbox("cfg-default", "default")
	srv := newTestServer(t, "http://127.0.0.1:8080", []runtime.Object{sb})
	stop := driveSessionRunning(t, srv, "u-alice")
	defer stop()

	session, errRes := srv.resolveToolSession(devCtx(), "default", "u-alice")
	if errRes != nil {
		t.Fatalf("expected provisioning to succeed, got error: %s", errText(errRes))
	}
	if session.Spec.SessionID != "u-alice" {
		t.Errorf("session id = %q, want u-alice", session.Spec.SessionID)
	}
	if session.Spec.SandboxID != "default" {
		t.Errorf("session sandboxId = %q, want default (the config)", session.Spec.SandboxID)
	}
	if session.Spec.Owner != "u-alice" {
		t.Errorf("session owner = %q, want u-alice", session.Spec.Owner)
	}
}

// A per-user session id with no config id cannot be created (config selection is explicit).
func TestResolveToolSession_CreateRequiresSandboxId(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:8080", nil)

	session, errRes := srv.resolveToolSession(devCtx(), "", "u-bob")
	if errRes == nil {
		t.Fatalf("expected error, got session %v", session)
	}
	if !strings.Contains(errText(errRes), "no sandbox id provided") {
		t.Errorf("error = %q, want it to mention missing sandbox id", errText(errRes))
	}
	if n := countSessions(t, srv); n != 0 {
		t.Errorf("created %d sessions, want 0", n)
	}
}

// Selecting a config that does not exist fails explicitly — no config is auto-created.
func TestResolveToolSession_CreateUnknownConfigFails(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:8080", nil)

	_, errRes := srv.resolveToolSession(devCtx(), "ghost", "u-carol")
	if errRes == nil {
		t.Fatal("expected error for unknown config")
	}
	if !strings.Contains(errText(errRes), `sandbox "ghost" not found`) {
		t.Errorf("error = %q, want \"sandbox \\\"ghost\\\" not found\"", errText(errRes))
	}
	if n := countSessions(t, srv); n != 0 {
		t.Errorf("created %d sessions, want 0", n)
	}
}

// Reuse: a Running session is returned as-is and no second session is created.
func TestResolveToolSession_ReusesRunningSession(t *testing.T) {
	sb := testSandbox("cfg-default", "default")
	sess := testSession("u-dave", "u-dave", "default", "127.0.0.1", 8080, boxyv1.SandboxPhaseRunning)
	srv := newTestServer(t, "http://127.0.0.1:8080", []runtime.Object{sb, sess})

	got, errRes := srv.resolveToolSession(devCtx(), "default", "u-dave")
	if errRes != nil {
		t.Fatalf("expected reuse to succeed, got error: %s", errText(errRes))
	}
	if got.Name != "u-dave" {
		t.Errorf("reused session = %q, want u-dave", got.Name)
	}
	if n := countSessions(t, srv); n != 1 {
		t.Errorf("session count = %d, want 1 (no recreate)", n)
	}
}

// A session id that belongs to a different config than the requested X-Sandbox-Id is rejected.
func TestResolveToolSession_SessionSandboxMismatch(t *testing.T) {
	sess := testSession("u-eve", "u-eve", "config-a", "127.0.0.1", 8080, boxyv1.SandboxPhaseRunning)
	srv := newTestServer(t, "http://127.0.0.1:8080", []runtime.Object{sess})

	_, errRes := srv.resolveToolSession(devCtx(), "config-b", "u-eve")
	if errRes == nil {
		t.Fatal("expected mismatch error")
	}
	if !strings.Contains(errText(errRes), "does not belong to sandbox") {
		t.Errorf("error = %q, want mismatch message", errText(errRes))
	}
}

// Preserved behavior: a Pending session is reused, not recreated (matches the pre-refactor path).
func TestEnsureSession_ReusesPendingWithoutRecreate(t *testing.T) {
	sb := testSandbox("cfg-default", "default")
	sess := testSession("p-sess", "p-sess", "default", "", 0, boxyv1.SandboxPhasePending)
	srv := newTestServer(t, "http://127.0.0.1:8080", []runtime.Object{sb, sess})

	gotSandbox, gotSession, err := srv.ensureSession(devCtx(), "p-sess", "default", "owner")
	if err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	if gotSandbox != "default" || gotSession != "p-sess" {
		t.Errorf("got (%q,%q), want (default,p-sess)", gotSandbox, gotSession)
	}
	if n := countSessions(t, srv); n != 1 {
		t.Errorf("session count = %d, want 1 (pending reused, not recreated)", n)
	}
}

// Preserved behavior: a Terminated session is deleted and recreated.
func TestEnsureSession_RecreatesTerminated(t *testing.T) {
	sb := testSandbox("cfg-default", "default")
	sess := testSession("t-sess", "t-sess", "default", "", 0, boxyv1.SandboxPhaseTerminated)
	srv := newTestServer(t, "http://127.0.0.1:8080", []runtime.Object{sb, sess})
	stop := driveSessionRunning(t, srv, "t-sess")
	defer stop()

	gotSandbox, gotSession, err := srv.ensureSession(devCtx(), "t-sess", "default", "owner")
	if err != nil {
		t.Fatalf("ensureSession should recreate terminated session: %v", err)
	}
	if gotSandbox != "default" || gotSession != "t-sess" {
		t.Errorf("got (%q,%q), want (default,t-sess)", gotSandbox, gotSession)
	}
	final, _ := srv.lookupSession(context.Background(), "t-sess")
	if final == nil || final.Status.Phase != boxyv1.SandboxPhaseRunning {
		t.Errorf("recreated session phase = %v, want Running", final)
	}
}
