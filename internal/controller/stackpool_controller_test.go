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
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

var _ = Describe("StackPool Controller", func() {
	var h *harness

	BeforeEach(func() { h = newHarness() })
	AfterEach(cleanup)

	It("fills the pool with warm Stacks built from its template", func() {
		pool := createPool(2, 5)
		h.settle()

		stacks := stacksOf(pool)
		Expect(stacks).To(HaveLen(2))
		for _, s := range stacks {
			Expect(s.Labels).To(HaveKeyWithValue("team", "ci"))
			Expect(s.Labels).To(HaveKeyWithValue(platformv1.LabelPool, pool.Name))
			Expect(s.Labels).To(HaveKey(platformv1.LabelTemplateHash))
			Expect(s.Spec.Foo).To(HaveValue(Equal("bar")))
			Expect(s.Spec.ExpiresAt).To(BeNil(), "idle Stacks must not age")

			// Deleting the pool cascades to its Stacks through the controller reference.
			Expect(s.OwnerReferences).To(ConsistOf(HaveField("UID", pool.UID)))
			Expect(s.OwnerReferences[0].Controller).To(HaveValue(BeTrue()))
		}
		Expect(get(pool.Name, pool).Status.Provisioning).To(BeEquivalentTo(2))
		Expect(meta.IsStatusConditionFalse(pool.Status.Conditions, platformv1.StackPoolConditionReady)).To(BeTrue())

		h.provision()

		get(pool.Name, pool)
		Expect(pool.Status.Warm).To(BeEquivalentTo(2))
		Expect(pool.Status.Provisioning).To(BeZero())
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, platformv1.StackPoolConditionReady)).To(BeTrue())
		Expect(stacksOf(pool)).To(HaveLen(2), "a full pool creates nothing more")
	})

	It("provisions on demand for pending claims, without exceeding max", func() {
		pool := createPool(1, 3)
		h.provision()

		for i := range 5 {
			createClaim(fmt.Sprintf("%s-claim-%d", pool.Name, i), pool.Name)
		}
		h.provision()
		h.provision()

		Expect(stacksOf(pool)).To(HaveLen(3))
		get(pool.Name, pool)
		Expect(pool.Status.Claimed).To(BeEquivalentTo(3))
		Expect(pool.Status.PendingClaims).To(BeEquivalentTo(2))
	})

	It("serves claims first come, first served", func() {
		pool := createPool(1, 1)
		h.provision()

		// Names sort the opposite way to creation, so only creation order explains the outcome.
		first := createClaim(pool.Name+"-z-first", pool.Name)
		time.Sleep(1100 * time.Millisecond) // creationTimestamp has second precision
		second := createClaim(pool.Name+"-a-second", pool.Name)
		h.settle()

		Expect(get(first.Name, first).Status.Phase).To(Equal(platformv1.StackClaimBound))
		Expect(get(second.Name, second).Status.Phase).To(Equal(platformv1.StackClaimPending))
	})

	It("never binds a Stack to two claims, nor a claim to two Stacks, under concurrent reconciles", func() {
		pool := createPool(3, 3)
		h.provision()
		for i := range 6 {
			createClaim(fmt.Sprintf("%s-claim-%d", pool.Name, i), pool.Name)
		}

		// Independent reconcilers race on the same pool, as replicas or
		// workers with stale state would.
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				defer GinkgoRecover()
				r := newPoolReconciler(h.clock)
				for range 5 {
					// Conflicts are the expected outcome of losing a race.
					_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: pool.Name}})
				}
			})
		}
		wg.Wait()
		h.settle()

		claimOf := map[types.UID]string{}
		for _, s := range stacksOf(pool) {
			Expect(s.Spec.ClaimRef).NotTo(BeNil())
			Expect(claimOf).NotTo(HaveKey(s.Spec.ClaimRef.UID), "claim bound to two Stacks")
			claimOf[s.Spec.ClaimRef.UID] = s.Name
		}
		Expect(claimOf).To(HaveLen(3))

		bound := 0
		for _, c := range list(&platformv1.StackClaimList{}).Items {
			if c.Status.StackName == "" {
				Expect(c.Status.Phase).To(Equal(platformv1.StackClaimPending))
				continue
			}
			bound++
			Expect(claimOf).To(HaveKeyWithValue(c.UID, c.Status.StackName), "claim and Stack disagree")
		}
		Expect(bound).To(Equal(3))
	})

	Context("when its view of the cluster is stale", func() {
		It("never hands a Stack to a second claim", func() {
			pool := createPool(1, 2)
			h.provision()
			stale := stacksOf(pool)[0] // seen unclaimed, before the first claim takes it
			first := createClaim(pool.Name+"-first", pool.Name)
			h.settle()
			Expect(get(first.Name, first).Status.StackName).To(Equal(stale.Name))

			second := get(createClaim(pool.Name+"-second", pool.Name).Name, &platformv1.StackClaim{})
			Expect(h.pools.bind(ctx, pool, second, &stale)).NotTo(Succeed())
			Expect(get(second.Name, second).Status.StackName).To(BeEmpty())

			h.provision()
			Expect(get(first.Name, first).Status.StackName).To(Equal(stale.Name))
			Expect(get(second.Name, second).Status.Phase).To(Equal(platformv1.StackClaimBound))
			Expect(second.Status.StackName).NotTo(Equal(stale.Name))
		})

		It("never gives a claim a second Stack", func() {
			pool := createPool(2, 3)
			h.provision()
			claim := createClaim(pool.Name+"-claim", pool.Name)
			stale := get(claim.Name, claim).DeepCopy() // seen pending, before it is bound
			h.settle()
			held := get(claim.Name, claim).Status.StackName
			other := unclaimed(stacksOf(pool))[0]

			Expect(h.pools.bind(ctx, pool, stale, &other)).NotTo(Succeed())
			h.settle()

			// The other Stack was taken before the claim refused it, and a
			// Stack's claimRef cannot be undone, so it is torn down.
			Expect(exists(other.Name, &platformv1.Stack{})).To(BeFalse())
			Expect(get(claim.Name, claim).Status.StackName).To(Equal(held))
			Expect(claim.Status.Phase).To(Equal(platformv1.StackClaimBound))
			Expect(exists(held, &platformv1.Stack{})).To(BeTrue())
		})
	})

	It("completes a binding interrupted after the Stack was taken", func() {
		pool := createPool(1, 1)
		h.provision()
		claim := createClaim(pool.Name+"-claim", pool.Name)
		stack := stacksOf(pool)[0]

		// The first write of a binding, without the second.
		update(&stack, func(s *platformv1.Stack) {
			s.Spec.ClaimRef = &platformv1.ClaimReference{Name: claim.Name, UID: claim.UID}
			s.Spec.ExpiresAt = &metav1.Time{Time: h.clock.Now().Add(time.Hour)}
		})
		h.settle()

		get(claim.Name, claim)
		Expect(claim.Status.Phase).To(Equal(platformv1.StackClaimBound))
		Expect(claim.Status.StackName).To(Equal(stack.Name))
		Expect(get(stack.Name, &stack).Spec.ExpiresAt).To(HaveValue(Equal(*claim.Status.ExpiresAt)))
	})

	It("trims surplus unclaimed Stacks, keeping the oldest", func() {
		pool := createPool(3, 3)
		h.provision()
		oldest := stacksOf(pool)[0].Name

		update(pool, func(p *platformv1.StackPool) { p.Spec.Warm = 1 })
		h.settle()

		Expect(stacksOf(pool)).To(ConsistOf(HaveField("Name", oldest)))
	})

	It("deletes a Stack whose claim disappeared without releasing it", func() {
		pool := createPool(1, 1)
		h.provision()
		claim := createClaim(pool.Name+"-claim", pool.Name)
		h.settle()
		stack := get(claim.Name, claim).Status.StackName

		// Bypass the finalizer, as a claim deleted mid-binding would.
		update(claim, func(c *platformv1.StackClaim) { c.Finalizers = nil })
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		h.reconcile(h.pools, pool.Name)

		Expect(exists(stack, &platformv1.Stack{})).To(BeFalse())
	})

	Context("when Stacks fail to provision", func() {
		failing := func(p *platformv1.StackPool) {
			p.Spec.Template.Metadata.Annotations = map[string]string{AnnotationSimulateFailure: "true"}
		}

		It("backs off, then halts provisioning until the template changes", func() {
			pool := createPool(1, 3, failing)

			for failures := 1; failures <= 3; failures++ {
				h.provision()
				get(pool.Name, pool)
				Expect(pool.Status.ConsecutiveFailures).To(BeEquivalentTo(failures))
				Expect(stacksOf(pool)).To(BeEmpty(), "failed Stacks are torn down and not replaced while backing off")

				h.clock.Step(30 * time.Second << (failures - 1))
			}

			h.clock.Step(time.Hour)
			h.settle()
			Expect(stacksOf(pool)).To(BeEmpty(), "the circuit breaker stops provisioning for good")
			get(pool.Name, pool)
			Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, platformv1.StackPoolConditionDegraded)).To(BeTrue())

			update(pool, func(p *platformv1.StackPool) { p.Spec.Template.Metadata.Annotations = nil })
			h.provision()

			get(pool.Name, pool)
			Expect(pool.Status.ConsecutiveFailures).To(BeZero())
			Expect(pool.Status.Warm).To(BeEquivalentTo(1))
			Expect(meta.IsStatusConditionFalse(pool.Status.Conditions, platformv1.StackPoolConditionDegraded)).To(BeTrue())
		})

		It("resets the failure count once a Stack provisions successfully", func() {
			pool := createPool(1, 3)
			h.provision()
			// Seed a failure history, as if two Stacks had just failed.
			get(pool.Name, pool)
			pool.Status.ConsecutiveFailures = 2
			pool.Status.LastFailureTime = new(now(h.clock))
			Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())

			// A claim takes the warm Stack, so the pool provisions a new one.
			createClaim(pool.Name+"-claim", pool.Name)
			h.clock.Step(time.Minute)
			h.provision()

			Expect(get(pool.Name, pool).Status.ConsecutiveFailures).To(BeZero())
		})
	})

	It("rolls out a template change surge-style, never touching claimed Stacks", func() {
		pool := createPool(1, 3)
		h.provision()
		claim := createClaim(pool.Name+"-claim", pool.Name)
		h.provision() // the claim takes the warm Stack and a replacement is provisioned
		claimed := get(claim.Name, claim).Status.StackName
		stale := unclaimed(stacksOf(pool))[0].Name

		update(pool, func(p *platformv1.StackPool) { p.Spec.Template.Spec.Foo = new("baz") })
		h.settle()

		// The stale Stack keeps serving claims until its replacement is ready.
		Expect(stacksOf(pool)).To(HaveLen(3))
		Expect(exists(stale, &platformv1.Stack{})).To(BeTrue())

		h.provision()

		stacks := stacksOf(pool)
		Expect(stacks).To(HaveLen(2))
		Expect(stacks).To(ContainElement(HaveField("Name", claimed)))
		Expect(unclaimed(stacks)).To(ConsistOf(HaveField("Spec.Foo", new("baz"))))
	})
})

var _ = Describe("planScaling", func() {
	stacks := func(n int) []*platformv1.Stack {
		out := make([]*platformv1.Stack, n)
		for i := range out {
			out[i] = &platformv1.Stack{}
			out[i].Name = fmt.Sprintf("s%d", i)
		}
		return out
	}
	pending := func(n int) []*platformv1.StackClaim { return make([]*platformv1.StackClaim, n) }

	DescribeTable("sizing",
		func(view *poolView, warm, maxStacks int32, canProvision bool, create, deletes int) {
			plan := planScaling(view, warm, maxStacks, canProvision)
			Expect(plan.create).To(Equal(create), "create")
			Expect(plan.delete).To(HaveLen(deletes), "delete")
		},
		Entry("empty pool fills to warm", &poolView{}, int32(2), int32(5), true, 2, 0),
		Entry("pending claims add demand", &poolView{pending: pending(2)}, int32(1), int32(5), true, 3, 0),
		Entry("max caps creation", &poolView{claimed: stacks(4), pending: pending(3)}, int32(1), int32(5), true, 1, 0),
		Entry("nothing is created while backing off", &poolView{}, int32(2), int32(5), false, 0, 0),
		Entry("surplus is trimmed", &poolView{ready: stacks(3)}, int32(1), int32(5), true, 0, 2),
		Entry("lowering max trims unclaimed Stacks only", &poolView{claimed: stacks(3), ready: stacks(2)}, int32(2), int32(3), true, 0, 2),
		Entry("stale provisioning Stacks are dropped", &poolView{staleProvisioning: stacks(2)}, int32(2), int32(5), true, 2, 2),
		Entry("stale ready Stacks cover until replaced", &poolView{staleReady: stacks(2)}, int32(2), int32(5), true, 2, 0),
		Entry("stale ready Stacks make room when max is tight", &poolView{staleReady: stacks(2)}, int32(2), int32(3), true, 2, 1),
		Entry("stale ready Stacks go once replacements are ready", &poolView{ready: stacks(2), staleReady: stacks(2)}, int32(2), int32(5), true, 0, 2),
	)

	It("trims provisioning Stacks before ready ones, newest first", func() {
		view := &poolView{ready: stacks(2), provisioning: stacks(2)}
		view.provisioning[0].Name, view.provisioning[1].Name = "p-old", "p-new"
		view.ready[0].Name, view.ready[1].Name = "r-old", "r-new"

		plan := planScaling(view, 1, 5, true)

		Expect(plan.delete).To(HaveExactElements(
			HaveField("Name", "p-new"), HaveField("Name", "p-old"), HaveField("Name", "r-new")))
	})
})
