package cliproxy

import (
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
)

func TestAdaptiveReserveRoutingConfig(t *testing.T) {
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "adaptive-reserve", HomeAccount: "home@example.test"}}
	state := normalizedRoutingRuntimeState(cfg)
	selector, ok := newRoutingSelector(state).(*coreauth.AdaptiveReserveSelector)
	if !ok || selector.HomeAccount != "home@example.test" {
		t.Fatalf("selector=%+v", selector)
	}
	cfg.Routing.HomeAccount = "changed@example.test"
	if state == normalizedRoutingRuntimeState(cfg) {
		t.Fatal("home changes must update runtime routing")
	}
}
