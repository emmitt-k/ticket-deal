// drainer.go implements the background goroutine that promotes queued
// users as the per-event token bucket refills.
//
// The drainer is a single process that discovers event queues dynamically:
// when a user enters an event that doesn't yet have a drainer running,
// a new sub-goroutine starts for that event. When an event's queue empties
// for more than 5 minutes, its sub-goroutine exits automatically.
//
// This means the drainer naturally scales to any number of concurrent
// events without any central registry of "active events."
package waitingroom

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/emmitt-k/ticket-deal/internal/auth"
	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// DrainerConfig holds the parameters for the drainer.
type DrainerConfig struct {
	// Redis client shared with the API handlers.
	RDB *redis.Client
	// WaitRoom is the per-event token bucket config. All events share
	// the same capacity and refill rate (set per-deployment, not per-event).
	WaitRoom redis.WaitRoomConfig
	// JWTSecret for issuing admission tokens to promoted users.
	JWTSecret []byte
	// TickInterval is how often the drainer re-checks each queue.
	// 100ms is the sweet spot: fast enough to not waste capacity,
	// slow enough to not hammer Redis with thousands of concurrent event loops.
	TickInterval time.Duration
}

// StartDrainer starts the background drainer. It returns immediately;
// the drainer runs in goroutines until ctx is cancelled.
func StartDrainer(ctx context.Context, cfg DrainerConfig) func() {
	if cfg.TickInterval == 0 {
		cfg.TickInterval = 100 * time.Millisecond
	}

	// active tracks the set of eventIDs that currently have a drain loop running.
	// Access is protected by the mutex.
	var (
		active   map[int64]struct{} = make(map[int64]struct{})
		mu       sync.Mutex
		cancelFns = make(map[int64]context.CancelFunc)
		wg        sync.WaitGroup
	)

	// onEvent is called by handlers when a user is queued. It starts
	// a drain loop for that event if one isn't already running.
	onEvent := func(eventID int64) {
		mu.Lock()
		_, running := active[eventID]
		if !running {
			active[eventID] = struct{}{}
			subCtx, cancel := context.WithCancel(ctx)
			cancelFns[eventID] = cancel
			wg.Add(1)
			go drainLoop(subCtx, &wg, eventID, cfg, &mu, active, cancelFns)
		}
		mu.Unlock()
	}

	// Wire into the queue package so handlers can notify the drainer
	// that a new event queue exists. This is the only coupling point:
	// queue.go calls back to drainer.go via this function variable.
	RegisterEventCallback(onEvent)

	// Cancel all sub-goroutines when the parent context is cancelled.
	go func() {
		<-ctx.Done()
		mu.Lock()
		for _, cancel := range cancelFns {
			cancel()
		}
		mu.Unlock()
		wg.Wait()
	}()

	return func() {} // drainer itself has no clean stop beyond ctx cancel
}

// drainLoop runs the drain cycle for a single event until ctx is cancelled
// or the queue is empty for 5 minutes.
func drainLoop(ctx context.Context, wg *sync.WaitGroup,
	eventID int64, cfg DrainerConfig,
	activeMu *sync.Mutex, active map[int64]struct{}, cancelFns map[int64]context.CancelFunc,
) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "drainLoop recovered panic",
				"event", eventID,
				"panic", r,
			)
		}
	}()

	ticker := time.NewTicker(cfg.TickInterval)
	defer ticker.Stop()

	const emptyTimeout = 5 * time.Minute
	lastHadQueue := time.Now()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			queueSize, err := redis.QueueSize(ctx, cfg.RDB, eventID)
			if err != nil {
				slog.WarnContext(ctx, "drain QueueSize failed",
					"event", eventID,
					"error", err,
				)
				continue
			}
			if queueSize == 0 {
				// Queue is empty. Exit after 5 minutes of staying empty.
				if time.Since(lastHadQueue) > emptyTimeout {
					cleanupDrainer(eventID, activeMu, active, cancelFns)
					return
				}
				continue
			}
			lastHadQueue = time.Now()

			// How many tokens (seats) became available since last tick?
			// refillRate is tokens/sec; tickInterval is the time since last check.
			// We cap at queueSize to avoid over-promoting.
			tokensToAdd := int(cfg.WaitRoom.RefillRate * cfg.TickInterval.Seconds())
			if tokensToAdd < 1 {
				tokensToAdd = 1 // always promote at least 1 if queue is non-empty
			}
			maxPromote := queueSize
			if tokensToAdd < maxPromote {
				maxPromote = tokensToAdd
			}

			for i := 0; i < maxPromote; i++ {
				userID, err := redis.ZPopMin(ctx, cfg.RDB, eventID)
				if err == redis.ErrQueueEmpty {
					break
				}
				if err != nil {
					slog.WarnContext(ctx, "ZPopMin failed",
						"event", eventID,
						"error", err,
					)
					break
				}

				// Issue JWT for this promoted user
				token, err := auth.Issue(userID, eventID, cfg.JWTSecret, 2*time.Minute)
				if err != nil {
					slog.ErrorContext(ctx, "Issue failed",
						"event", eventID,
						"user", userID,
						"error", err,
					)
					continue
				}

				// Deliver JWT to the user's SSE goroutine
				if ok := Deliver(eventID, userID, token); !ok {
					slog.WarnContext(ctx, "user has no active SSE stream",
						"event", eventID,
						"user", userID,
					)
				}
			}
		}
	}
}

// cleanupDrainer removes the drainer from the active set and calls its cancel func.
func cleanupDrainer(eventID int64,
	activeMu *sync.Mutex, active map[int64]struct{}, cancelFns map[int64]context.CancelFunc,
) {
	activeMu.Lock()
	delete(active, eventID)
	if cancel, ok := cancelFns[eventID]; ok {
		cancel()
		delete(cancelFns, eventID)
	}
	activeMu.Unlock()
}
