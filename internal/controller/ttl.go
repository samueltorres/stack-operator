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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// leaseTTL is how long a claim may hold its Stack under a pool's rules.
type leaseTTL struct {
	duration time.Duration
	// clamped is true when the claim asked for more than the pool's maxTTL.
	clamped bool
}

// effectiveTTL resolves the claim's ttl against the pool's defaultTTL and maxTTL.
func effectiveTTL(claim *platformv1.StackClaim, pool *platformv1.StackPool) leaseTTL {
	ttl := leaseTTL{duration: pool.Spec.DefaultTTL.Duration}
	if claim.Spec.TTL != nil {
		ttl.duration = claim.Spec.TTL.Duration
	}
	if maxTTL := pool.Spec.MaxTTL; maxTTL != nil && ttl.duration > maxTTL.Duration {
		ttl = leaseTTL{duration: maxTTL.Duration, clamped: true}
	}
	return ttl
}

// expiresAt is when a lease that started at boundAt runs out.
func (t leaseTTL) expiresAt(boundAt metav1.Time) metav1.Time {
	return metav1.NewTime(boundAt.Add(t.duration))
}

// now is the current time at the precision the API stores it with. Lease
// times are computed from stored timestamps, so using the same precision
// keeps a recomputed expiry identical to the original one.
func now(clk clock.PassiveClock) metav1.Time {
	return metav1.NewTime(clk.Now().Truncate(time.Second))
}
