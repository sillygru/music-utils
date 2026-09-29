package httpserver

import (
	"context"
	"log/slog"
	"time"

	"github.com/sillygru/music-utils/internal/pacer"
)

// pacerDegradationInterval is how often the shared pacers are checked for
// having fallen back to private pacing.
//
// The check is two in-memory reads, so the interval is chosen to keep a
// long-running outage visible in the log rather than to be cheap.
const pacerDegradationInterval = 30 * time.Second

// watchPacerDegradation logs when a shared upstream pacer has reverted to pacing
// locally, and again when it recovers.
//
// The failure is otherwise invisible: the server keeps serving at the configured
// interval, so nothing looks wrong while the live process and any background job
// have stopped taking turns and are together hitting the upstream twice as hard
// as intended.
func watchPacerDegradation(ctx context.Context, logger *slog.Logger, pacers ...*pacer.Shared) {
	if logger == nil {
		return
	}
	ticker := time.NewTicker(pacerDegradationInterval)
	defer ticker.Stop()

	// Track what was already reported so a persistent outage logs once, and
	// recovery is logged once, rather than every tick.
	reported := make(map[*pacer.Shared]bool, len(pacers))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, shared := range pacers {
				if shared == nil {
					continue
				}
				degraded, reason := shared.Degraded()
				if degraded == reported[shared] {
					continue
				}
				if degraded {
					logger.Warn("shared upstream pacing unavailable, pacing locally until it recovers; "+
						"a background job will no longer take turns with live traffic",
						"reason", reason)
				} else {
					logger.Info("shared upstream pacing recovered")
				}
				reported[shared] = degraded
			}
		}
	}
}
