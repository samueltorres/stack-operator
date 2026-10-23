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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// claimPoolNameField indexes StackClaims by the pool they claim from. The key
// matches the CRD's selectable field, so the same field selector works against
// both the informer cache and the API server.
const claimPoolNameField = "spec.poolName"

// SetupFieldIndexes registers the cache indexes the controllers rely on. It
// must run once, before the controllers are started.
func SetupFieldIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &platformv1.StackClaim{}, claimPoolNameField, func(obj client.Object) []string {
		return []string{obj.(*platformv1.StackClaim).Spec.PoolName}
	})
}

// claimsInPool returns the StackClaims that claim from the named pool.
func claimsInPool(ctx context.Context, c client.Reader, poolName string) ([]platformv1.StackClaim, error) {
	var claims platformv1.StackClaimList
	if err := c.List(ctx, &claims, client.MatchingFields{claimPoolNameField: poolName}); err != nil {
		return nil, err
	}
	return claims.Items, nil
}

// optimisticMergeFrom builds a merge patch that only applies if the object has
// not changed since base was read. Every write that depends on what the
// controller observed goes through it, so decisions made on a stale cache are
// rejected with a conflict and retried instead of overwriting newer state.
func optimisticMergeFrom(base client.Object) client.Patch {
	return client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
}
