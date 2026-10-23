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

func createStack(name string, mutate ...func(*platformv1.Stack)) *platformv1.Stack {
	GinkgoHelper()
	stack := &platformv1.Stack{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, m := range mutate {
		m(stack)
	}
	Expect(k8sClient.Create(ctx, stack)).To(Succeed())
	return stack
}

var _ = Describe("Stack simulator", func() {
	var h *harness

	BeforeEach(func() { h = newHarness() })
	AfterEach(cleanup)

	readyReason := func(stack *platformv1.Stack) string {
		GinkgoHelper()
		return meta.FindStatusCondition(get(stack.Name, stack).Status.Conditions, platformv1.StackConditionReady).Reason
	}

	It("provisions a Stack after the provisioning delay", func() {
		stack := createStack("simulated")
		h.settle()
		Expect(readyReason(stack)).To(Equal(platformv1.StackReasonProvisioning))

		h.provision()
		Expect(readyReason(stack)).To(Equal(platformv1.StackReasonProvisioned))
		Expect(stack.IsReady()).To(BeTrue())
	})

	It("fails a Stack on request", func() {
		stack := createStack("simulated-failure", func(s *platformv1.Stack) {
			s.Annotations = map[string]string{AnnotationSimulateFailure: "true"}
		})
		h.provision()

		Expect(readyReason(stack)).To(Equal(platformv1.StackReasonFailed))
		Expect(stack.HasFailed()).To(BeTrue())
	})
})

var _ = Describe("Stack expiry", func() {
	var h *harness

	BeforeEach(func() { h = newHarness() })
	AfterEach(cleanup)

	It("deletes a Stack once it expires", func() {
		stack := createStack("expiring", func(s *platformv1.Stack) {
			s.Spec.ExpiresAt = &metav1.Time{Time: h.clock.Now().Add(24 * time.Hour)}
		})

		h.clock.Step(23 * time.Hour)
		h.settle()
		Expect(exists(stack.Name, stack)).To(BeTrue())

		h.clock.Step(time.Hour)
		h.settle()
		Expect(exists(stack.Name, stack)).To(BeFalse())
	})

	It("never deletes a Stack without an expiry", func() {
		stack := createStack("forever")

		h.clock.Step(365 * 24 * time.Hour)
		h.settle()

		Expect(exists(stack.Name, stack)).To(BeTrue())
	})

	It("rejects rebinding a claimed Stack", func() {
		stack := createStack("bound", func(s *platformv1.Stack) {
			s.Spec.ClaimRef = &platformv1.ClaimReference{Name: "a", UID: "uid-a"}
		})

		stack.Spec.ClaimRef = &platformv1.ClaimReference{Name: "b", UID: "uid-b"}
		Expect(k8sClient.Update(ctx, stack)).To(MatchError(ContainSubstring("claimRef is immutable")))

		get(stack.Name, stack).Spec.ClaimRef = nil
		Expect(k8sClient.Update(ctx, stack)).To(MatchError(ContainSubstring("claimRef is immutable")))
	})
})
