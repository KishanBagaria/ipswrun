#!/bin/bash
# exchange.sh <work dir>: provisions 13.0's adid in one run and asks it for a one-time password.
# The guest (guests/provision.c) starts provisioning and waits; this script takes its client
# challenge to Apple with the provision tool and puts the answer in the guest's home.
#
# Needs in $TOOLS: ipswrun (set IPSWRUN to choose another build, such as one with the fixed
# clock), provision (this directory's tool), provision-guest (the guest, cross-built), a cache
# directory, and AppleIncRootCertificate.cer. Responses are kept in $LIVE and replayed from
# there, so with FIXEDCLOCK set and a patched ipswrun a finished exchange repeats with no request.
set -e
: "${TOOLS:?set TOOLS}" "${LIVE:?set LIVE to the directory responses are kept in}"
work=$1; home=$work/home; state=$work/fpsd
rm -rf "$home"; mkdir -p "$home" "$state"
provision() { "$TOOLS/provision" --dir "$LIVE" --root "$TOOLS/AppleIncRootCertificate.cer" --authkit 1 --akd 1.0 --cfnetwork 1399.4 --darwin 22.1.0 "$@"; }
provision start
base64 -d "$LIVE/spim.b64" > "$home/spim.bin"
"${IPSWRUN:-$TOOLS/ipswrun}" $TRACE --stream-compressed --cache "$TOOLS/cache" --home "$home" --mount /var/db/fpsd="$state" \
    --daemon com.apple.adid=system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid 13.0 "$TOOLS/provision-guest" $GUEST_ARGS > "$work/guest.out" 2>&1 &
for _ in $(seq 120); do [ -f "$home/cpim.bin" ] && break; sleep 0.5; done
[ -f "$home/cpim.bin" ] || { cat "$work/guest.out"; echo "adid gave no client challenge"; exit 1; }
base64 -w0 "$home/cpim.bin" > "$work/cpim.b64"
provision finish "$work/cpim.b64"
base64 -d "$LIVE/ptm.b64" > "$home/ptm.bin"
base64 -d "$LIVE/tk.b64" > "$home/tk.bin"
touch "$home/done"
wait
grep -v '^  \[' "$work/guest.out"
ls -la "$state/adi"
