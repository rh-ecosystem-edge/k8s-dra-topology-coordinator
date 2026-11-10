#!/usr/bin/env bash

: "${DEMO_HOST:?Set DEMO_HOST to the SSH target for the H100 node}"

kubectl() {
	ssh -o BatchMode=yes "$DEMO_HOST" kubectl "$@"
}

topoctl() {
	ssh -o BatchMode=yes "$DEMO_HOST" topoctl "$@"
}

dra-inspect() {
	ssh -o BatchMode=yes "$DEMO_HOST" dra-inspect "$@"
}
