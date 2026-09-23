package main

import (
	"sync"
	"time"
)

// banStore is a runtime IP ban list. Zero expiry means permanent.
type banStore struct {
	mu   sync.Mutex
	bans map[string]time.Time
}

func newBanStore() *banStore {
	return &banStore{bans: make(map[string]time.Time)}
}

func (b *banStore) ban(ip string, until time.Time) {
	b.mu.Lock()
	b.bans[ip] = until
	b.mu.Unlock()
}

func (b *banStore) unban(ip string) bool {
	b.mu.Lock()
	_, ok := b.bans[ip]
	delete(b.bans, ip)
	b.mu.Unlock()
	return ok
}

func (b *banStore) isBanned(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.bans[ip]
	if !ok {
		return false
	}
	if !until.IsZero() && time.Now().After(until) {
		delete(b.bans, ip)
		return false
	}
	return true
}

// active returns the currently banned IPs, pruning expired entries.
func (b *banStore) active() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	out := make(map[string]string, len(b.bans))
	for ip, until := range b.bans {
		if !until.IsZero() && now.After(until) {
			delete(b.bans, ip)
			continue
		}
		if until.IsZero() {
			out[ip] = "permanent"
		} else {
			out[ip] = until.UTC().Format(time.RFC3339)
		}
	}
	return out
}
