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
	"cmp"
	"encoding/json"
	"hash/fnv"
	"slices"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// poolView is a pool's Stacks and claims, sorted into the buckets allocation
// and scaling decisions are made from. Stacks being torn down are left out:
// they no longer count towards anything.
//
// Every Stack slice is ordered oldest first.
type poolView struct {
	// claimed Stacks are bound to a claim, including half-bound ones.
	claimed []*platformv1.Stack

	// Unclaimed Stacks built from the current template.
	ready        []*platformv1.Stack
	provisioning []*platformv1.Stack

	// Unclaimed Stacks built from a previous template, awaiting replacement.
	staleReady        []*platformv1.Stack
	staleProvisioning []*platformv1.Stack

	// failed Stacks are unclaimed Stacks that failed to provision.
	failed []*platformv1.Stack

	// orphaned Stacks point at a claim that does not hold them: the claim is
	// gone or going, or it holds another Stack.
	orphaned []*platformv1.Stack

	// halfBound pairs a Stack that points at a claim with that claim, which
	// has not recorded the Stack yet.
	halfBound []binding

	// pending claims wait for a Stack, oldest first. Half-bound claims are not
	// pending.
	pending []*platformv1.StackClaim
}

type binding struct {
	claim *platformv1.StackClaim
	stack *platformv1.Stack
}

// classify sorts a pool's Stacks and claims into a poolView. hash is the hash
// of the pool's current template.
func classify(hash string, stacks []platformv1.Stack, claims []platformv1.StackClaim) *poolView {
	view := &poolView{}
	byCreation := func(a, b metav1.ObjectMeta) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Name, b.Name))
	}

	claimsByUID := make(map[types.UID]*platformv1.StackClaim, len(claims))
	for i := range claims {
		claimsByUID[claims[i].UID] = &claims[i]
	}
	beingBound := map[types.UID]bool{}

	slices.SortFunc(stacks, func(a, b platformv1.Stack) int { return byCreation(a.ObjectMeta, b.ObjectMeta) })
	for i := range stacks {
		stack := &stacks[i]
		current := stack.Labels[platformv1.LabelTemplateHash] == hash

		switch {
		case !stack.DeletionTimestamp.IsZero():
			// Being torn down: no longer part of the pool.
		case stack.Spec.ClaimRef != nil:
			claim := claimsByUID[stack.Spec.ClaimRef.UID]
			switch {
			case claim != nil && claim.Status.StackName == stack.Name:
				view.claimed = append(view.claimed, stack)
			case claim != nil && claim.IsPending() && !beingBound[claim.UID]:
				beingBound[claim.UID] = true
				view.claimed = append(view.claimed, stack)
				view.halfBound = append(view.halfBound, binding{claim: claim, stack: stack})
			default:
				view.orphaned = append(view.orphaned, stack)
			}
		case stack.HasFailed():
			view.failed = append(view.failed, stack)
		case stack.IsReady() && current:
			view.ready = append(view.ready, stack)
		case stack.IsReady():
			view.staleReady = append(view.staleReady, stack)
		case current:
			view.provisioning = append(view.provisioning, stack)
		default:
			view.staleProvisioning = append(view.staleProvisioning, stack)
		}
	}

	for _, claim := range claimsByUID {
		if claim.IsPending() && !beingBound[claim.UID] {
			view.pending = append(view.pending, claim)
		}
	}
	slices.SortFunc(view.pending, func(a, b *platformv1.StackClaim) int { return byCreation(a.ObjectMeta, b.ObjectMeta) })
	return view
}

// readySince reports whether any Stack built from the template with the given
// hash became ready after t.
func (v *poolView) readySince(hash string, t time.Time) bool {
	for _, bucket := range [][]*platformv1.Stack{v.claimed, v.ready} {
		for _, stack := range bucket {
			ready := meta.FindStatusCondition(stack.Status.Conditions, platformv1.StackConditionReady)
			if stack.Labels[platformv1.LabelTemplateHash] == hash && stack.IsReady() && ready.LastTransitionTime.After(t) {
				return true
			}
		}
	}
	return false
}

// remove drops a Stack from whichever unclaimed bucket holds it.
func (v *poolView) remove(stack *platformv1.Stack) {
	for _, bucket := range []*[]*platformv1.Stack{&v.ready, &v.provisioning, &v.staleReady, &v.staleProvisioning} {
		*bucket = slices.DeleteFunc(*bucket, func(s *platformv1.Stack) bool { return s.UID == stack.UID })
	}
}

// scalePlan is what the pool should do to reach its desired size.
type scalePlan struct {
	create int
	delete []*platformv1.Stack
}

// planScaling decides how many Stacks to create and which unclaimed Stacks to
// delete, so that the pool holds warm unclaimed Stacks plus one for every
// claim still pending, without ever running more than max Stacks. Claimed
// Stacks are never touched.
//
// Surplus Stacks are trimmed cheapest first: provisioning before ready, newest
// before oldest. Stacks from a previous template are replaced surge-style: a
// stale ready Stack stays until a current one can take its place, so claims
// keep being served during a rollout.
func planScaling(view *poolView, warm, maxStacks int32, canProvision bool) scalePlan {
	var plan scalePlan

	claimed := len(view.claimed)
	desired := int(warm) + len(view.pending)
	current := len(view.ready) + len(view.provisioning)

	// Trim current Stacks beyond what is desired or what fits under max.
	trim := min(current, max(0, current-desired, claimed+current-int(maxStacks)))
	trimOrder := slices.Concat(newestFirst(view.provisioning), newestFirst(view.ready))
	plan.delete = append(plan.delete, trimOrder[:trim]...)
	current -= trim
	currentReady := min(len(view.ready), current)

	free := int(maxStacks) - claimed - current
	if canProvision {
		plan.create = min(max(0, desired-current), free)
	}

	// Stale Stacks never get a slot a current Stack needs. Provisioning ones
	// are thrown away right away; ready ones cover for current Stacks that are
	// not ready yet.
	plan.delete = append(plan.delete, view.staleProvisioning...)
	keep := min(max(0, desired-currentReady), free-plan.create, len(view.staleReady))
	keep = max(0, keep)
	plan.delete = append(plan.delete, view.staleReady[:len(view.staleReady)-keep]...)

	return plan
}

func newestFirst(stacks []*platformv1.Stack) []*platformv1.Stack {
	s := slices.Clone(stacks)
	slices.Reverse(s)
	return s
}

// templateHash identifies a pool template, so Stacks built from an older
// version can be told apart. encoding/json sorts map keys, which keeps the
// hash stable.
func templateHash(tmpl *platformv1.StackTemplateSpec) string {
	b, err := json.Marshal(tmpl)
	if err != nil {
		panic(err) // A template is plain data; marshaling cannot fail.
	}
	h := fnv.New32a()
	_, _ = h.Write(b)
	return rand.SafeEncodeString(strconv.FormatUint(uint64(h.Sum32()), 10))
}
