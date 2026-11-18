#!/bin/sh
# Generates shell completions and the man page for release packages (GoReleaser before hook).
set -e
rm -rf completions manpages
mkdir -p completions manpages
go build -o /tmp/kubeweave-gen .
for sh in bash zsh fish powershell; do /tmp/kubeweave-gen completion "$sh" > "completions/kubeweave.$sh"; done
/tmp/kubeweave-gen man | gzip -9 -n > manpages/kubeweave.1.gz
rm -f /tmp/kubeweave-gen
