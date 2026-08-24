# Thin delegation to run.sh, which is the single source of truth and the form
# actually exercised on the dev host (this machine has no make, so the Makefile
# path is verified inside the golang container instead).
.PHONY: help test race lifecycle all vet build live-block process-recover clean
help:            ; @./run.sh help
test:            ; @./run.sh test
race:            ; @./run.sh race
lifecycle:       ; @./run.sh lifecycle
all:             ; @./run.sh all
vet:             ; @./run.sh vet
build:           ; @./run.sh build
live-block:      ; @./run.sh live-block
process-recover: ; @./run.sh process-recover
clean:           ; @rm -f rzp-guard.exe gate-verify.exe rzp-guard-testhook.exe rzp-guard-operator.exe && rm -rf .gotmp evidence/live
