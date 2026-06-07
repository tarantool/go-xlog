package follow

import (
	"context"
	"fmt"
	"time"
)

// pollWatcher is the default Watcher: it simply waits one poll interval, leaving
// the follower to re-read and re-resolve. It adds no dependency and behaves
// correctly on every filesystem (including network / overlay / container mounts
// where event-based watchers miss events). The path argument is unused — the
// follower decides what to re-check.
type pollWatcher struct {
	interval time.Duration
}

// Wait blocks for one poll interval or until ctx is done.
func (w pollWatcher) Wait(ctx context.Context, _ string) error {
	t := time.NewTimer(w.interval)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("follow: poll: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}
