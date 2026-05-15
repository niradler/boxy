package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
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

func userFromContext(ctx context.Context) *authv1.UserInfo {
	u, _ := ctx.Value(authUserKey).(*authv1.UserInfo)
	return u
}
