// pacer.go — 每认证 token 1 req/s 限速器（未公开驱动端点的突发平滑）。
package main

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

const puterRequestInterval = time.Second

type puterPacerEntry struct {
	next        time.Time
	wake        chan struct{}
	timerActive bool
	waiters     int
	lastUsed    time.Time
}

var puterRequestPacer = struct {
	sync.Mutex
	entries map[[32]byte]*puterPacerEntry
}{entries: make(map[[32]byte]*puterPacerEntry)}

// waitForPuterRequestSlot 平滑突发请求。限制按认证 token 计，独立账号可并发推进。
// 每 token 至多一个 timer：排队调用共享 wake channel，避免为同一 deadline 分配 N 个
// timer 造成 thundering herd。
func waitForPuterRequestSlot(ctx context.Context, authToken string) error {
	key := sha256.Sum256([]byte(strings.TrimSpace(authToken)))
	for {
		now := time.Now()
		puterRequestPacer.Lock()
		entry := puterRequestPacer.entries[key]
		if entry == nil {
			entry = &puterPacerEntry{wake: make(chan struct{}), lastUsed: now}
			puterRequestPacer.entries[key] = entry
		}
		entry.lastUsed = now
		if !entry.next.After(now) {
			entry.next = now.Add(puterRequestInterval)
			cleanupPuterPacerLocked(now)
			puterRequestPacer.Unlock()
			return nil
		}

		wake := entry.wake
		entry.waiters++
		if !entry.timerActive {
			entry.timerActive = true
			deadline := entry.next
			time.AfterFunc(time.Until(deadline), func() { wakePuterPacerEntry(key, entry, deadline) })
		}
		puterRequestPacer.Unlock()

		select {
		case <-ctx.Done():
			puterRequestPacer.Lock()
			if current := puterRequestPacer.entries[key]; current == entry && entry.waiters > 0 {
				entry.waiters--
			}
			puterRequestPacer.Unlock()
			return ctx.Err()
		case <-wake:
			puterRequestPacer.Lock()
			if current := puterRequestPacer.entries[key]; current == entry && entry.waiters > 0 {
				entry.waiters--
			}
			puterRequestPacer.Unlock()
			// 竞争刚打开的槽位：只有推进 next 的 goroutine 返回，取消的等待者不占未来容量。
		}
	}
}

func wakePuterPacerEntry(key [32]byte, expected *puterPacerEntry, deadline time.Time) {
	puterRequestPacer.Lock()
	defer puterRequestPacer.Unlock()
	entry := puterRequestPacer.entries[key]
	if entry != expected || !entry.timerActive || !entry.next.Equal(deadline) {
		return
	}
	entry.timerActive = false
	close(entry.wake)
	entry.wake = make(chan struct{})
}

func cleanupPuterPacerLocked(now time.Time) {
	if len(puterRequestPacer.entries) <= 256 {
		return
	}
	cutoff := now.Add(-time.Minute)
	for key, entry := range puterRequestPacer.entries {
		if entry.waiters == 0 && !entry.timerActive && entry.lastUsed.Before(cutoff) {
			delete(puterRequestPacer.entries, key)
		}
	}
}
