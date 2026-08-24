# Thin delegation to run.sh, which is the single source of truth and the form
# actually exercised on the dev host (this machine has no make, so the Makefile
# path is verified inside the golang container instead).
.PHONY: help test race vet build live-block process-recover clean
help:            ; @./run.sh help
test:            ; @./run.sh test
race:            ; @./run.sh race
vet:             ; @./run.sh vet
build:           ; @./run.sh build
live-block:      ; @./run.sh live-block
process-recover: ; @./run.sh process-recover
clean:           ; @rm -f rzp-guard.exe gate-verify.exe rzp-guard-testhook.exe && rm -rf .gotmp evidence/live
