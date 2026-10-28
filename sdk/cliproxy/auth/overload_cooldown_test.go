package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestUpstreamOverloadDoesNotCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	model := "claude-opus-5-5"
	cases := []struct {
		name string
		err  *Error
	}{
		{name: "stream error event", err: &Error{HTTPStatus: http.StatusBadGateway, Message: `{"type":"error","error":{"details":null,"type":"overloaded_error","message":"Overloaded"},"request_id":"req_1"}`}},
		{name: "http 529", err: &Error{HTTPStatus: 529, Message: "Overloaded"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			auth := &Auth{ID: "auth-overload-" + uuid.NewString(), Provider: "claude"}
			if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}
			m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Error: tc.err})
			assertNoCooldown(t, m, auth.ID, model)
		})
	}
}

func TestUpstreamOverloadOtherBadGatewayStillCoolsDown(t *testing.T) {
	err := &Error{HTTPStatus: http.StatusBadGateway, Message: `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`}
	if shouldSkipCredentialCooldown(err) {
		t.Fatal("a non-overload 502 must still cool the credential")
	}
}
