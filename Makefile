# Thin delegation to run.sh, which is the single source of truth and is the
# form actually exercised on the dev host (this machine has no make).
.PHONY: help test race vet build live live-block live-recover clean
help:          ; @./run.sh help
test:          ; @./run.sh test
race:          ; @./run.sh race
vet:           ; @./run.sh vet
build:         ; @./run.sh build
live:          ; @./run.sh live
live-block:    ; @./run.sh live-block
live-recover:  ; @./run.sh live-recover
clean:         ; @rm -f rzp-guard.exe && rm -rf .gotmp evidence/live
