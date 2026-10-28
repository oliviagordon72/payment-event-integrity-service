package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

func (s *Store) Work(ctx context.Context) {
	for ctx.Err() == nil {
		l, err := s.Claim(ctx, 30*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("claim failed", "error", err)
			}
		} else if l != nil {
			jobCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = s.Process(jobCtx, l)
			cancel()
			if err != nil && !errors.Is(err, ErrLeaseLost) {
				slog.Warn("processing failed", "event", l.EventID, "attempt", l.Attempts, "error", err)
				if failErr := s.Fail(ctx, l, err); failErr != nil && ctx.Err() == nil {
					slog.Error("retry update failed", "error", failErr)
				}
			}
			continue
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
