#!/bin/sh
# Generates shell completions and the man page for release packages (GoReleaser before hook).
set -e
rm -rf completions manpages
mkdir -p completions manpages
go build -o /tmp/kubesage-gen .
for sh in bash zsh fish powershell; do /tmp/kubesage-gen completion "$sh" > "completions/kubesage.$sh"; done
/tmp/kubesage-gen man | gzip -9 -n > manpages/kubesage.1.gz
rm -f /tmp/kubesage-gen
