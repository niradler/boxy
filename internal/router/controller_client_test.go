package router_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"boxy.dev/boxy/internal/router"
)

func TestControllerClient_HealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := router.NewControllerClient(router.ControllerClientConfig{
		MTLSDisabled: true,
	})
	resp, err := client.RawClient().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
