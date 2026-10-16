#!/bin/sh
# Prints the secret NAME from .secrets/NAME, generating a random value on first use.
# Used by .env as a Dagger secret provider:  FOO=cmd://"sh scripts/secret.sh foo"
set -eu
name="${1:?usage: secret.sh NAME}"
case "$name" in *[!a-z0-9_]*) echo "invalid secret name: $name" >&2; exit 1 ;; esac
dir="$(cd "$(dirname "$0")/.." && pwd)/.secrets"
umask 077
mkdir -p "$dir" && chmod 700 "$dir"
file="$dir/$name"
if [ ! -s "$file" ]; then
  LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 >"$file"
fi
cat "$file"
