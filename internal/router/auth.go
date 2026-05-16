package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type cachedResult struct {
	user     authv1.UserInfo
	storedAt time.Time
}

// tokenReviewer validates SA bearer tokens via TokenReview, caching results.
// The raw token is never stored - only its SHA-256 fingerprint is used as the cache key.
type tokenReviewer struct {
	cs       kubernetes.Interface
	ttl      time.Duration
	devToken string // optional static bypass for local dev / e2e

	mu      sync.RWMutex
	entries map[string]*cachedResult
}

func newTokenReviewer(ctx context.Context, cs kubernetes.Interface, ttl time.Duration, devToken string) *tokenReviewer {
	tr := &tokenReviewer{
		cs:       cs,
		ttl:      ttl,
		devToken: devToken,
		entries:  make(map[string]*cachedResult),
	}
	go tr.evictLoop(ctx)
	return tr
}

func (tr *tokenReviewer) authenticate(ctx context.Context, token string) (*authv1.UserInfo, error) {
	if tr.devToken != "" && token == tr.devToken {
		u := authv1.UserInfo{Username: "dev-token"}
		return &u, nil
	}

	key := tokenHash(token)

	tr.mu.RLock()
	if e, ok := tr.entries[key]; ok && time.Since(e.storedAt) < tr.ttl {
		u := e.user
		tr.mu.RUnlock()
		return &u, nil
	}
	tr.mu.RUnlock()

	rev, err := tr.cs.AuthenticationV1().TokenReviews().Create(ctx,
		&authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token}},
		metav1.CreateOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("tokenreview: %w", err)
	}
	if !rev.Status.Authenticated {
		msg := rev.Status.Error
		if msg == "" {
			msg = "not authenticated"
		}
		return nil, fmt.Errorf("%s", msg)
	}

	tr.mu.Lock()
	tr.entries[key] = &cachedResult{user: rev.Status.User, storedAt: time.Now()}
	tr.mu.Unlock()

	return &rev.Status.User, nil
}

func (tr *tokenReviewer) authorizeResource(
	ctx context.Context,
	user *authv1.UserInfo,
	namespace string,
	verb string,
	resource string,
	name string,
) error {
	if user == nil || user.Username == "" {
		return fmt.Errorf("missing authenticated user")
	}
	if tr.devToken != "" && user.Username == "dev-token" {
		return nil
	}

	sar, err := tr.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   user.Username,
			UID:    user.UID,
			Groups: user.Groups,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace,
				Group:     "boxy.dev",
				Resource:  resource,
				Verb:      verb,
				Name:      name,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("subjectaccessreview: %w", err)
	}
	if !sar.Status.Allowed {
		reason := sar.Status.Reason
		if reason == "" {
			reason = "access denied"
		}
		return fmt.Errorf("%s", reason)
	}
	return nil
}

func (s *Server) requireResourceAccess(w http.ResponseWriter, r *http.Request, verb, resource, name string) bool {
	user, ok := r.Context().Value(authUserKey).(*authv1.UserInfo)
	if !ok || user == nil {
		s.jsonErr(w, http.StatusUnauthorized, "unauthorized", "")
		return false
	}
	if err := s.auth.authorizeResource(r.Context(), user, s.cfg.SandboxNamespace, verb, resource, name); err != nil {
		s.log.Debug("authorization failed", "user", user.Username, "verb", verb, "resource", resource, "name", name, "err", err)
		s.jsonErr(w, http.StatusForbidden, "forbidden", "authorization")
		return false
	}
	return true
}

func (s *Server) canResourceAccess(ctx context.Context, verb, resource, name string) error {
	user, ok := ctx.Value(authUserKey).(*authv1.UserInfo)
	if !ok || user == nil {
		return fmt.Errorf("unauthorized")
	}
	return s.auth.authorizeResource(ctx, user, s.cfg.SandboxNamespace, verb, resource, name)
}

// evictLoop removes stale entries; exits when ctx is cancelled.
func (tr *tokenReviewer) evictLoop(ctx context.Context) {
	tick := time.NewTicker(tr.ttl * 2)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			tr.mu.Lock()
			for k, e := range tr.entries {
				if time.Since(e.storedAt) >= tr.ttl {
					delete(tr.entries, k)
				}
			}
			tr.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// The raw token is never retained in memory beyond this call.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type ctxKey string

const authUserKey ctxKey = "authUser"
