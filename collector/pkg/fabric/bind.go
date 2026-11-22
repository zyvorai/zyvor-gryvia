// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"sync"
	"time"
)

const (
	// BindTTL is how long a pid -> job answer (found or not) is reused before the
	// resolver is asked again. It is short so a recycled pid is re-checked quickly.
	BindTTL = 5 * time.Second
	// MaxBindings bounds the pid cache (and so Folder's pid -> job map).
	MaxBindings = 4096
)

// ResolveFunc maps a host PID to the (namespace, job) of the pod that owns it. It
// must fail closed: false when the pid is gone, not in a pod, or the pod is not
// a known job pod. flight.Resolver provides it (see main).
type ResolveFunc func(pid uint32) (namespace, job string, ok bool)

type binding struct {
	bound   bool
	expires time.Time
}

// Binder attributes fabric signals to jobs. Before a signal is folded it makes
// sure Folder's pid binding is the resolver's current answer: found -> Bind,
// not found -> Unbind (the signal lands under "_unattributed/<comm>"). It never
// guesses, and it keeps at most MaxBindings pids.
//
// Add must be called from one goroutine (the fabric signal loop).
type Binder struct {
	Folder  *Folder
	Resolve ResolveFunc

	mu    sync.Mutex
	cache map[uint32]binding
	now   func() time.Time
}

// NewBinder creates a Binder. A nil resolver leaves every signal unattributed.
func NewBinder(f *Folder, r ResolveFunc) *Binder {
	return &Binder{Folder: f, Resolve: r, cache: map[uint32]binding{}, now: time.Now}
}

// Add folds the signal under its job when the pid resolves, else unattributed.
func (b *Binder) Add(s Signal) {
	b.bind(s.PID)
	b.Folder.Add(s)
}

func (b *Binder) bind(pid uint32) {
	if b.Resolve == nil || pid == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if c, ok := b.cache[pid]; ok && now.Before(c.expires) {
		return
	}
	if len(b.cache) >= MaxBindings {
		b.evictLocked(now)
	}
	ns, job, ok := b.Resolve(pid)
	if ok && ns != "" && job != "" {
		b.Folder.Bind(pid, ns, job)
	} else {
		ok = false
		b.Folder.Unbind(pid)
	}
	b.cache[pid] = binding{bound: ok, expires: now.Add(BindTTL)}
}

// evictLocked drops expired entries and, if that frees nothing, every entry, so
// the maps stay bounded under pid churn. Dropped pids are unbound: they are
// re-resolved on their next signal.
func (b *Binder) evictLocked(now time.Time) {
	for pid, c := range b.cache {
		if !now.Before(c.expires) {
			b.Folder.Unbind(pid)
			delete(b.cache, pid)
		}
	}
	if len(b.cache) < MaxBindings {
		return
	}
	for pid := range b.cache {
		b.Folder.Unbind(pid)
		delete(b.cache, pid)
	}
}

// Len is the number of cached pids.
func (b *Binder) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.cache)
}
