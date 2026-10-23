#!/usr/bin/env bash
# Leases a Stack for the duration of a test run, the way a CI job would.
#
#   ./03-ci-job.sh                       # runs a placeholder test command
#   ./03-ci-job.sh make test-e2e         # runs your own command
#
# The leased Stack's name is exported as STACK_NAME for the command.
set -euo pipefail

cd "$(dirname "$0")"
test_cmd=("$@")
if [ ${#test_cmd[@]} -eq 0 ]; then
  test_cmd=(bash -c 'echo "Running e2e tests against Stack $STACK_NAME"; sleep 5')
fi

claim=$(kubectl create -f 02-ci-claim.yaml -o name)
echo "Created $claim"

# Release the Stack however the job ends: success, failure or Ctrl-C.
trap 'echo "Releasing $claim"; kubectl delete "$claim" --wait=false' EXIT

if ! kubectl wait --for=condition=Bound "$claim" --timeout=20m; then
  kubectl get "$claim" -o jsonpath='{.status.conditions[?(@.type=="Bound")].message}{"\n"}' >&2
  exit 1
fi

STACK_NAME=$(kubectl get "$claim" -o jsonpath='{.status.stackName}')
export STACK_NAME
echo "Leased Stack $STACK_NAME until $(kubectl get "$claim" -o jsonpath='{.status.expiresAt}')"

"${test_cmd[@]}"
