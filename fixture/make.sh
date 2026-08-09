#!/bin/sh
# Build the multi-party fixture: three speakers, three voices, one sentence each,
# synthesized by the same hanzoai/speech that transcribes them back.
#
# Ogg opus, because that is what a browser publishes and what the attendant
# decodes — the fixture goes through the room's own path, not a shortcut.
#
# SPEECH defaults to a port-forward of svc/speech in namespace hanzo.
set -eu
SPEECH=${SPEECH:-http://127.0.0.1:8799}
cd "$(dirname "$0")"

say() { # voice, file, words
	curl -sS -m 120 -X POST "$SPEECH/v1/audio/speech" \
		-H 'content-type: application/json' \
		-d "{\"model\":\"kokoro\",\"voice\":\"$1\",\"response_format\":\"opus\",\"input\":\"$3\"}" \
		-o "$2.ogg"
	printf '%s\t%s\t%s bytes\n' "$2" "$1" "$(wc -c <"$2.ogg")"
}

say af_heart  ana  "The index rebuild finished at four this morning, and every shard is reporting green across both regions."
say am_michael ben "Then we can cut the release today, as long as the migration test passes on the second cluster."
say af_bella  cleo "I still owe you the capacity numbers, and I would rather not guess at them in front of everyone."
say am_michael nod "Mhm. Yeah."
