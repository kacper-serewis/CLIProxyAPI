package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

type connectionRefreshExecutor struct {
	schedulerProviderTestExecutor
	err error
}

func (e *connectionRefreshExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	if e.err != nil {
		return nil, e.err
	}
	a = a.Clone()
	a.Metadata["expired"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	a.Metadata["access_token"] = "renewed"
	return a, nil
}

func TestRefreshConnectionRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		fast bool
	}{
		{"dns", fmt.Errorf("oauth refresh: %w", &net.DNSError{Err: "no such host", Name: "example.invalid"}), true},
		{"dial", fmt.Errorf("oauth refresh: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("network unreachable")}), true},
		{"server", errors.New("upstream 503 service unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, &RoundRobinSelector{}, nil)
			e := &connectionRefreshExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, err: tc.err}
			m.RegisterExecutor(e)
			a := &Auth{ID: "boot", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "expired", "refresh_token": "refresh", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339)}}
			if _, err := m.Register(ctx, a); err != nil {
				t.Fatal(err)
			}
			for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second} {
				if _, err := m.ForceRefreshAuth(ctx, a.ID); err == nil {
					t.Fatal("expected connection failure")
				}
				got, _ := m.GetByID(a.ID)
				if !tc.fast {
					want = refreshFailureBackoff
				}
				if delay := got.NextRefreshAfter.Sub(got.UpdatedAt); delay != want {
					t.Fatalf("attempt %d: delay %v, want %v", i+1, delay, want)
				}
				if !got.Unavailable || got.RequiresSignIn {
					t.Fatal("must remain temporarily unavailable without requiring sign-in")
				}
				next, ok := m.nextRefreshCheckAt(got.UpdatedAt, got, 15*time.Minute)
				if !ok || !next.Equal(got.NextRefreshAfter) {
					t.Fatal("retry must be scheduled before normal 15 minute interval")
				}
				if !m.shouldRefresh(got, next) {
					t.Fatal("credential must be eligible at retry deadline")
				}
			}
			e.err = nil
			if _, err := m.ForceRefreshAuth(ctx, a.ID); err != nil {
				t.Fatal(err)
			}
			got, _ := m.GetByID(a.ID)
			if got.Unavailable || got.Status != StatusActive || got.RefreshFailures != 0 || !got.NextRefreshAfter.IsZero() {
				t.Fatal("successful recovery must clear error and backoff")
			}
		})
	}
}
