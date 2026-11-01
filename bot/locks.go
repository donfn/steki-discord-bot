package bot

import "sync"

// KeyedMutex hands out one mutex per key. Entries are created on demand and
// dropped once nobody holds or waits for them, so the map does not grow with
// every member the bot has ever seen. The zero value is ready to use.
type KeyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refMutex
}

type refMutex struct {
	sync.Mutex
	refs int // holders plus waiters, guarded by KeyedMutex.mu
}

// Lock blocks until key is free and returns the function that releases it.
func (k *KeyedMutex) Lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*refMutex{}
	}
	m := k.locks[key]
	if m == nil {
		m = &refMutex{}
		k.locks[key] = m
	}
	m.refs++
	k.mu.Unlock()

	m.Lock()
	return func() {
		m.Unlock()

		k.mu.Lock()
		m.refs--
		if m.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
