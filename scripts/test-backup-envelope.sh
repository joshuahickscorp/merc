#!/usr/bin/env bash
set -euo pipefail

for command_name in age age-keygen tar; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "backup envelope test requires $command_name" >&2
    exit 1
  }
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/cx-backup-envelope.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/payload/database" "$WORK/payload/objects"
printf '%s\n' 'synthetic database payload' > "$WORK/payload/database/dump.sql"
printf '%s\n' 'synthetic object payload' > "$WORK/payload/objects/object.txt"

age-keygen -o "$WORK/identity.txt" 2> "$WORK/keygen.log"
RECIPIENT="$(awk '/^# public key:/ {print $4}' "$WORK/identity.txt")"
case "$RECIPIENT" in age1*) ;; *) echo "age recipient generation failed" >&2; exit 1 ;; esac

tar -C "$WORK" -cf "$WORK/plain.tar" payload
age -r "$RECIPIENT" -o "$WORK/backup.tar.age" "$WORK/plain.tar"
rm "$WORK/plain.tar"
test -s "$WORK/backup.tar.age"

age -d -i "$WORK/identity.txt" -o "$WORK/restored.tar" "$WORK/backup.tar.age"
mkdir "$WORK/restored"
tar -C "$WORK/restored" -xf "$WORK/restored.tar"
cmp "$WORK/payload/database/dump.sql" "$WORK/restored/payload/database/dump.sql"
cmp "$WORK/payload/objects/object.txt" "$WORK/restored/payload/objects/object.txt"

printf '%s\n' 'PASS age-encrypted backup envelope round-trip'
