package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenMiddleware_RejectsWithoutToken(t *testing.T) {
	s := &server{cfg: &config{controllerToken: "secret"}}

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := s.tokenMiddleware(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/exec", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("inner handler must not be called when token is missing")
	}
}

func TestTokenMiddleware_AcceptsCorrectToken(t *testing.T) {
	s := &server{cfg: &config{controllerToken: "secret"}}

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := s.tokenMiddleware(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/exec", nil)
	req.Header.Set("X-Boxy-Controller-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Error("inner handler must be called with correct token")
	}
}

func TestTokenMiddleware_RejectsWrongToken(t *testing.T) {
	s := &server{cfg: &config{controllerToken: "secret"}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := s.tokenMiddleware(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/exec", nil)
	req.Header.Set("X-Boxy-Controller-Token", "wrong")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestTokenMiddleware_HealthzBypassesToken(t *testing.T) {
	s := &server{cfg: &config{controllerToken: "secret"}}

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := s.tokenMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("/healthz must bypass token check")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from healthz, got %d", w.Code)
	}
}

func TestTokenMiddleware_DisabledWhenEmpty(t *testing.T) {
	s := &server{cfg: &config{controllerToken: ""}}

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := s.tokenMiddleware(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/exec", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("middleware must be a no-op when controllerToken is empty")
	}
}
