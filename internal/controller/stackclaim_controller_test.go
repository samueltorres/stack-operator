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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

var _ = Describe("StackClaim Controller", func() {
	var (
		h     *harness
		pool  *platformv1.StackPool
		claim *platformv1.StackClaim
	)

	BeforeEach(func() {
		h = newHarness()
		pool = createPool(1, 2, func(p *platformv1.StackPool) {
			p.Spec.MaxTTL = &metav1.Duration{Duration: 4 * time.Hour}
		})
		h.provision()
	})
	AfterEach(cleanup)

	// claimBound creates a claim and lets the pool bind it.
	claimBound := func(ttl ...time.Duration) {
		GinkgoHelper()
		claim = createClaim(pool.Name+"-claim", pool.Name, ttl...)
		h.settle()
		Expect(get(claim.Name, claim).Status.Phase).To(Equal(platformv1.StackClaimBound))
	}
	stackOf := func(c *platformv1.StackClaim) *platformv1.Stack {
		GinkgoHelper()
		return get(c.Status.StackName, &platformv1.Stack{})
	}

	It("binds to a warm Stack and starts the lease clock", func() {
		claimBound()

		Expect(claim.Finalizers).To(ContainElement(claimFinalizer))
		Expect(meta.IsStatusConditionTrue(claim.Status.Conditions, platformv1.StackClaimConditionBound)).To(BeTrue())
		Expect(claim.Status.BoundAt.Time).To(BeTemporally("==", h.clock.Now()))
		Expect(claim.Status.ExpiresAt.Time).To(Equal(h.clock.Now().Add(2*time.Hour)), "the pool's defaultTTL applies")

		stack := stackOf(claim)
		Expect(stack.Spec.ClaimRef).To(HaveValue(Equal(platformv1.ClaimReference{Name: claim.Name, UID: claim.UID})))
		Expect(stack.Spec.ExpiresAt).To(HaveValue(Equal(*claim.Status.ExpiresAt)))
	})

	It("clamps the ttl to the pool's maxTTL", func() {
		claimBound(10 * time.Hour)

		Expect(claim.Status.ExpiresAt.Time).To(BeTemporally("==", claim.Status.BoundAt.Add(4*time.Hour)))
		Expect(meta.IsStatusConditionTrue(claim.Status.Conditions, platformv1.StackClaimConditionTTLClamped)).To(BeTrue())
	})

	It("extends the lease when the ttl is raised, counting from when it was bound", func() {
		claimBound(time.Hour)
		h.clock.Step(30 * time.Minute)

		update(claim, func(c *platformv1.StackClaim) { c.Spec.TTL = &metav1.Duration{Duration: 3 * time.Hour} })
		h.settle()

		want := claim.Status.BoundAt.Add(3 * time.Hour)
		Expect(get(claim.Name, claim).Status.ExpiresAt.Time).To(BeTemporally("==", want))
		Expect(stackOf(claim).Spec.ExpiresAt.Time).To(BeTemporally("==", want))
	})

	It("overrides manual edits of the Stack's expiry", func() {
		claimBound()
		stack := stackOf(claim)
		update(stack, func(s *platformv1.Stack) { s.Spec.ExpiresAt = &metav1.Time{Time: h.clock.Now().Add(1000 * time.Hour)} })

		h.settle()

		Expect(stackOf(claim).Spec.ExpiresAt).To(HaveValue(Equal(*claim.Status.ExpiresAt)))
	})

	It("tears the Stack down when the lease expires", func() {
		claimBound(time.Hour)
		stack := claim.Status.StackName

		h.clock.Step(time.Hour)
		h.settle()

		Expect(exists(stack, &platformv1.Stack{})).To(BeFalse())
		Expect(get(claim.Name, claim).Status.Phase).To(Equal(platformv1.StackClaimExpired))
		Expect(meta.IsStatusConditionFalse(claim.Status.Conditions, platformv1.StackClaimConditionBound)).To(BeTrue())
	})

	It("releases the Stack when the claim is deleted", func() {
		claimBound()
		stack := claim.Status.StackName

		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		h.settle()

		Expect(exists(claim.Name, &platformv1.StackClaim{})).To(BeFalse())
		Expect(exists(stack, &platformv1.Stack{})).To(BeFalse())
	})

	It("is lost, and never re-bound, when its Stack disappears", func() {
		claimBound()
		stack := claim.Status.StackName

		Expect(k8sClient.Delete(ctx, stackOf(claim))).To(Succeed())
		h.provision()

		get(claim.Name, claim)
		Expect(claim.Status.Phase).To(Equal(platformv1.StackClaimLost))
		Expect(claim.Status.StackName).To(Equal(stack))
		Expect(get(pool.Name, pool).Status.Warm).To(BeEquivalentTo(1), "the pool has Stacks, but a lost claim takes none")
	})

	It("is lost when its Stack fails", func() {
		claimBound()
		stack := stackOf(claim)
		meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
			Type: platformv1.StackConditionReady, Status: metav1.ConditionFalse, Reason: platformv1.StackReasonFailed,
		})
		Expect(k8sClient.Status().Update(ctx, stack)).To(Succeed())

		h.settle()

		Expect(get(claim.Name, claim).Status.Phase).To(Equal(platformv1.StackClaimLost))
		Expect(meta.FindStatusCondition(claim.Status.Conditions, platformv1.StackClaimConditionBound).Reason).
			To(Equal(platformv1.StackClaimReasonStackFailed))
	})

	It("waits while its pool does not exist", func() {
		claim = createClaim("orphan-claim", "no-such-pool")
		h.settle()

		get(claim.Name, claim)
		Expect(claim.Status.Phase).To(Equal(platformv1.StackClaimPending))
		Expect(meta.FindStatusCondition(claim.Status.Conditions, platformv1.StackClaimConditionBound).Reason).
			To(Equal(platformv1.StackClaimReasonPoolNotFound))
	})

	It("rejects changing the pool", func() {
		claim = createClaim(pool.Name+"-claim", pool.Name)
		get(claim.Name, claim)
		claim.Spec.PoolName = "other"

		Expect(k8sClient.Update(ctx, claim)).To(MatchError(ContainSubstring("poolName is immutable")))
	})
})
