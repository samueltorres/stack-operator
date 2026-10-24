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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

// poolLabel names the StackPool a metric is about.
const poolLabel = "pool"

var (
	poolStacks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stackpool_stacks",
		Help: "Number of Stacks in a StackPool by state (provisioning, warm, claimed).",
	}, []string{poolLabel, "state"})

	poolDesiredWarm = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stackpool_desired_warm",
		Help: "Number of warm Stacks a StackPool currently aims for, after warm schedules.",
	}, []string{poolLabel})

	poolPendingClaims = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stackpool_pending_claims",
		Help: "Number of StackClaims waiting for a Stack from a StackPool.",
	}, []string{poolLabel})

	poolConsecutiveFailures = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stackpool_consecutive_failures",
		Help: "Stacks from the current template of a StackPool that failed to provision in a row.",
	}, []string{poolLabel})

	// claimWaitSeconds is the number to watch when sizing spec.warm: claims
	// served by a warm Stack bind in seconds, the others wait for provisioning.
	claimWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "stackclaim_wait_seconds",
		Help:    "Time from a StackClaim's creation until it was bound to a Stack.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 13), // 1s … ~68m
	}, []string{poolLabel})
)

func init() {
	metrics.Registry.MustRegister(poolStacks, poolDesiredWarm, poolPendingClaims, poolConsecutiveFailures, claimWaitSeconds)
}

func recordPoolMetrics(pool *platformv1.StackPool) {
	s := pool.Status
	poolStacks.WithLabelValues(pool.Name, "provisioning").Set(float64(s.Provisioning))
	poolStacks.WithLabelValues(pool.Name, "warm").Set(float64(s.Warm))
	poolStacks.WithLabelValues(pool.Name, "claimed").Set(float64(s.Claimed))
	poolDesiredWarm.WithLabelValues(pool.Name).Set(float64(s.DesiredWarm))
	poolPendingClaims.WithLabelValues(pool.Name).Set(float64(s.PendingClaims))
	poolConsecutiveFailures.WithLabelValues(pool.Name).Set(float64(s.ConsecutiveFailures))
}

func forgetPoolMetrics(poolName string) {
	labels := prometheus.Labels{poolLabel: poolName}
	poolStacks.DeletePartialMatch(labels)
	poolDesiredWarm.DeletePartialMatch(labels)
	poolPendingClaims.DeletePartialMatch(labels)
	poolConsecutiveFailures.DeletePartialMatch(labels)
	claimWaitSeconds.DeletePartialMatch(labels)
}
