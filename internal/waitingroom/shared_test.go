package waitingroom

import (
	"context"
	"testing"

	"github.com/emmitt-k/ticket-deal/internal/redis"
)

// testClient is a package-level helper available to all *_test.go files
// in this package. It connects to localhost:6379 and skips the test if
// Redis is unavailable so CI without Redis still passes.
func testClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(redis.Config{Addr: "localhost:6379"})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis unavailable at localhost:6379: %v", err)
	}
	return c
}
