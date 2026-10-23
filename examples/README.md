# Examples

Each file shows one way to use the operator. Stacks are provisioned by the
built-in simulator, so everything below works on a throwaway
[kind](https://kind.sigs.k8s.io/) cluster.

| File | Shows |
|---|---|
| [`01-ci-pool.yaml`](01-ci-pool.yaml) | A pool that keeps Stacks warm for CI |
| [`02-ci-claim.yaml`](02-ci-claim.yaml) | The claim a CI job creates to lease a Stack |
| [`03-ci-job.sh`](03-ci-job.sh) | A CI job: lease a Stack, run tests, release it |
| [`04-on-demand-pool.yaml`](04-on-demand-pool.yaml) | A pool with nothing warm, and a claim whose ttl gets clamped |
| [`05-engineer-stack.yaml`](05-engineer-stack.yaml) | Stacks created by hand, with and without an expiry |
| [`06-failing-pool.yaml`](06-failing-pool.yaml) | Provisioning backoff and the circuit breaker |

## Setup

```sh
kind create cluster --name stacks
make install                                           # CRDs
go run ./cmd/main.go --simulated-provisioning-delay=30s  # leave running
```

Run the rest in another terminal, from the repository root. Keep an eye on
everything with:

```sh
kubectl get stackpools,stackclaims,stacks
```

## Walkthrough

**Warm pool.** Three Stacks appear and become ready about 30s later:

```sh
kubectl apply -f examples/01-ci-pool.yaml
kubectl get stackpool ci-e2e -w
```

**A CI run.** The job gets a warm Stack straight away, the pool provisions a
replacement, and the Stack is torn down when the job ends:

```sh
examples/03-ci-job.sh
```

Run several at once to see claims queue once the pool hits `max`:

```sh
for i in $(seq 1 20); do examples/03-ci-job.sh sleep 60 & done; wait
```

**On demand, with a clamped ttl.** The claim waits for provisioning, then
holds its Stack for 12h rather than the 24h it asked for:

```sh
kubectl apply -f examples/04-on-demand-pool.yaml
kubectl wait --for=condition=Bound stackclaim/training-run --timeout=5m
kubectl get stackclaim training-run \
  -o jsonpath='{.status.conditions[?(@.type=="TTLClamped")].message}{"\n"}'
```

Extend a lease by raising its ttl (still capped by `maxTTL`, and always
counted from when the claim was bound):

```sh
kubectl patch stackclaim training-run --type=merge -p '{"spec":{"ttl":"8h"}}'
```

**Hand-made Stacks.**

```sh
kubectl apply -f examples/05-engineer-stack.yaml
```

**Circuit breaker.**

```sh
kubectl apply -f examples/06-failing-pool.yaml
kubectl get stackpool broken-template -w
kubectl describe stackpool broken-template   # conditions and events
```

## Cleanup

```sh
kubectl delete stackclaims --all
kubectl delete stackpools --all   # cascades to the pools' Stacks
kubectl delete stacks --all
kind delete cluster --name stacks
```
