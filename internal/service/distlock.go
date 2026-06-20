package service

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// distLock is a heartbeat-based distributed lock backed by Redis.
//
// Unlike a plain SETNX with a long TTL, this uses a short base TTL (e.g. 30–60 s)
// that a background goroutine renews every baseTTL/3. If the holder process is
// killed, the heartbeat stops and Redis expires the key after at most baseTTL.
// This collapses the deadlock window from "hours" to "tens of seconds".
//
// Safe release uses a Lua fencing script so a slow holder cannot delete a lock
// that was already re-acquired by another instance.
type distLock struct {
	client  *redis.Client
	key     string
	token   string
	baseTTL time.Duration
	cancel  context.CancelFunc
}

// acquireDistLock tries to take the lock. Returns (lock, true, nil) on success.
// Returns (nil, false, nil) if another holder owns it.
// Returns (nil, false, err) on Redis error.
func acquireDistLock(client *redis.Client, key string, baseTTL time.Duration) (*distLock, bool, error) {
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	ok, err := client.SetNX(context.Background(), key, token, baseTTL).Result()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	hbCtx, cancel := context.WithCancel(context.Background())
	lock := &distLock{
		client:  client,
		key:     key,
		token:   token,
		baseTTL: baseTTL,
		cancel:  cancel,
	}
	go lock.heartbeat(hbCtx)
	return lock, true, nil
}

func (l *distLock) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(l.baseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = l.client.Expire(context.Background(), l.key, l.baseTTL)
		}
	}
}

// releaseLockScript deletes the key only when its value matches our token.
var releaseLockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
else
    return 0
end
`)

// release stops the heartbeat and atomically deletes the Redis key.
func (l *distLock) release() {
	l.cancel()
	_ = releaseLockScript.Run(context.Background(), l.client, []string{l.key}, l.token).Err()
}
