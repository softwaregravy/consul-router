package main

import (
	"runtime"
	"sync"
	"time"
)

// The cache type is an implementation of a resolver decorator that caches
// service endpoints returned by a base resolver using a LRU cache.
type cache struct {
	// Immutable fields of the cache.
	timeout time.Duration
	rslv    resolver
	done    chan struct{}

	// Mutable fields of the cache, the mutex must be locked to access them
	// concurrently.
	mutex sync.RWMutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	sync.RWMutex
	srv []service
	err error
	exp time.Time
}

func cached(timeout time.Duration, rslv resolver) *cache {
	c := &cache{
		timeout: timeout,
		rslv:    rslv,
		done:    make(chan struct{}),
		cache:   make(map[string]*cacheEntry),
	}

	// The use of a finalizer on the cache object gives us the ability to clear
	// the internal goroutine without requiring an explictly API to do so.
	runtime.SetFinalizer(c, func(c *cache) { close(c.done) })

	// It's important that this goroutine doesn't reference the cache object
	// itself, otherwise it would never get garbage collected.
	go cacheVacuum(&c.mutex, c.cache, c.done)
	return c
}

func (c *cache) resolve(name string) (srv []service, err error) {
	now := time.Now()

	for {
		if e := c.lookup(name, now); e != nil {
			if now.After(e.exp) {
				c.remove(name, e)
			} else {
				e.RLock()
				srv = e.srv
				err = e.err
				e.RUnlock()
				break
			}
		}

		e := &cacheEntry{exp: now.Add(c.timeout)}
		e.Lock()

		if !c.add(name, e) {
			continue
		}

		srv, err = c.rslv.resolve(name)
		e.srv = srv
		e.err = err
		e.Unlock()
		break
	}

	// Making a copy is important, the caller becomes the owner of the returned
	// service list and may modify its content. We don't want this to affect the
	// content of the cache.
	srv = copyServices(srv)
	return
}

func (c *cache) lookup(name string, now time.Time) *cacheEntry {
	c.mutex.RLock()
	entry := c.cache[name]
	c.mutex.RUnlock()
	return entry
}

func (c *cache) add(name string, entry *cacheEntry) (ok bool) {
	c.mutex.Lock()

	if c.cache[name] == nil {
		ok = true
		c.cache[name] = entry
	}

	c.mutex.Unlock()
	return
}

func (c *cache) remove(name string, entry *cacheEntry) {
	c.mutex.Lock()

	// Ensure the entry wasn't changed since the last time it was pulled out of
	// the map.
	if c.cache[name] == entry {
		delete(c.cache, name)
	}

	c.mutex.Unlock()
}

func cacheVacuum(mutex *sync.RWMutex, cache map[string]*cacheEntry, done <-chan struct{}) {
	// This constant is used to limit the maximum number of cache entries
	// visited during one vaccum pass to avoid locking the mutex for too
	// long when the cache is large.
	// Because iterating over maps is randomized this should still give
	// eventual consistency and evict stale entries from the cache.
	const max = 100

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			cacheVacuumPass(mutex, cache, now, max)
		}
	}
}

func cacheVacuumPass(mutex *sync.RWMutex, cache map[string]*cacheEntry, now time.Time, max int) {
	i := 0
	mutex.Lock()

	for name, entry := range cache {
		if i++; i > max {
			break
		}
		if now.After(entry.exp) {
			delete(cache, name)
		}
	}

	mutex.Unlock()
}
