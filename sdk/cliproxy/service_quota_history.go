package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// startQuotaHistory records credential quota observations next to the auth files so the
// management panel can chart pooled quota across restarts.
func (s *Service) startQuotaHistory(ctx context.Context, cfg *config.Config) {
	if s.coreManager == nil || cfg == nil {
		return
	}
	authDir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err != nil || authDir == "" {
		log.Warnf("quota history disabled: cannot resolve auth-dir: %v", err)
		return
	}
	s.coreManager.StartQuotaHistory(ctx, coreauth.QuotaHistoryPath(authDir))
}
