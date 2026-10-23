/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// expectationsTimeout bounds how long an unobserved mutation blocks a pool.
// It only matters if a watch event is lost; normally the cache catches up in
// milliseconds.
const expectationsTimeout = 2 * time.Minute

// expectations remembers the Stack creations and deletions a pool has issued
// but its informer cache has not caught up with yet, in the spirit of the
// ReplicaSet controller's expectations.
//
// Without them a reconcile running against a stale cache would not see the
// Stacks it just created and would create more, or would count the same
// failed Stack twice. Provisioning a Stack takes many minutes and costs real
// money, so the pool refuses to scale until its cache reflects its own writes.
type expectations struct {
	clock clock.PassiveClock

	mu    sync.Mutex
	pools map[string]*poolExpectations
}

type poolExpectations struct {
	creates map[string]time.Time    // Stack name → when it was created
	deletes map[types.UID]time.Time // Stack UID → when it was deleted
}

func newExpectations(clk clock.PassiveClock) *expectations {
	return &expectations{clock: clk, pools: map[string]*poolExpectations{}}
}

// expectCreate records that the pool created the named Stack.
func (e *expectations) expectCreate(pool string, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forPool(pool).creates[name] = e.clock.Now()
}

// expectDelete records that the pool deleted the Stack with the given UID.
func (e *expectations) expectDelete(pool string, uid types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forPool(pool).deletes[uid] = e.clock.Now()
}

// satisfied reconciles the pool's expectations with the Stacks currently in
// the cache and reports whether every mutation has been observed. A creation is
// observed once the Stack shows up; a deletion once the Stack is terminating or
// gone. Expectations older than expectationsTimeout are dropped.
func (e *expectations) satisfied(pool string, stacks []platformv1.Stack) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	p, ok := e.pools[pool]
	if !ok {
		return true
	}

	live := make(map[types.UID]bool, len(stacks))
	names := make(map[string]bool, len(stacks))
	for i := range stacks {
		names[stacks[i].Name] = true
		live[stacks[i].UID] = stacks[i].DeletionTimestamp.IsZero()
	}

	now := e.clock.Now()
	for name, at := range p.creates {
		if names[name] || now.Sub(at) > expectationsTimeout {
			delete(p.creates, name)
		}
	}
	for uid, at := range p.deletes {
		if !live[uid] || now.Sub(at) > expectationsTimeout {
			delete(p.deletes, uid)
		}
	}

	if len(p.creates) == 0 && len(p.deletes) == 0 {
		delete(e.pools, pool)
		return true
	}
	return false
}

// forget drops every expectation of a pool, for example once it is deleted.
func (e *expectations) forget(pool string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.pools, pool)
}

func (e *expectations) forPool(pool string) *poolExpectations {
	p, ok := e.pools[pool]
	if !ok {
		p = &poolExpectations{creates: map[string]time.Time{}, deletes: map[types.UID]time.Time{}}
		e.pools[pool] = p
	}
	return p
}
