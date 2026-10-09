#!/usr/bin/env bash
# Publishes the lexicons in lexicons/garden/engram so other servers can find
# them.
#
# An account's server looks up the space type declaration (garden.engram.space)
# of every `space:` scope it is asked for, to name it on the consent screen.
# When it can't find one, the official Bluesky PDS refuses the sign-in with
# "invalid_scope: Unable to retrieve space declarations".
#
# A schema is found in two steps:
#   1. the DNS TXT record _lexicon.engram.garden holds `did=<a DID>`
#   2. that account's repo holds a com.atproto.lexicon.schema record whose
#      key is the NSID (garden.engram.space, ...)
#
# So before running this, add the TXT record for the account that will
# publish (Cloudflare, for engram.garden):
#   name  _lexicon.engram.garden
#   value did=<the account's DID>
#
# Usage:
#   GOAT_USERNAME=hailey.at GOAT_PASSWORD=<app password> scripts/publish-lexicons.sh
#   scripts/publish-lexicons.sh --dry-run     # check the DNS and show the steps
#
# Needs goat (go install github.com/bluesky-social/goat@latest), jq and curl.
# Run it again after changing a lexicon: it updates what changed.
set -euo pipefail
cd "$(dirname "$0")/.."

DOMAIN=engram.garden
LEXICONS=lexicons/garden/engram
SPACE_NSID=garden.engram.space
DID=${LEXICON_DID:-did:plc:oisofpd7lj26yvgiivf3lxsi} # hailey.at
DRY_RUN=0
[[ ${1:-} == --dry-run ]] && DRY_RUN=1

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
run() {
  if ((DRY_RUN)); then say "  would run: $*"; else "$@"; fi
}

for tool in goat jq curl; do
  command -v "$tool" >/dev/null || die "$tool isn't installed"
done

# 1. The TXT record names the account that will publish.
# It asks the domain's own nameserver, so a "not found" cached by the
# resolver on this machine can't hold it up.
txt_records() {
  if command -v dig >/dev/null; then
    local ns
    ns=$(dig +short NS "$DOMAIN" | head -1)
    dig +short TXT "_lexicon.$DOMAIN" ${ns:+@"$ns"}
  else
    curl -fsS "https://dns.google/resolve?name=_lexicon.$DOMAIN&type=TXT" | jq -r '.Answer[]?.data'
  fi
}
say "Checking _lexicon.$DOMAIN"
if ! txt_records | tr -d '"' | grep -qx "did=$DID"; then
  say "  not set to did=$DID yet. Found: $(txt_records | tr '\n' ' ')"
  if ((DRY_RUN)); then
    say "  (dry run: carrying on)"
  else
    die "the nameserver for $DOMAIN has no TXT record _lexicon.$DOMAIN. In the DNS dashboard for $DOMAIN add a TXT record with Name _lexicon (the zone's name is added for you) and Content did=$DID, then run this again"
  fi
else
  say "  ok: did=$DID"
fi

# 2. Sign in as that account.
if ((DRY_RUN)); then
  say "Would sign in as ${GOAT_USERNAME:-<GOAT_USERNAME>}"
else
  [[ -n ${GOAT_USERNAME:-} && -n ${GOAT_PASSWORD:-} ]] || die "set GOAT_USERNAME and GOAT_PASSWORD (an app password)"
  who=$GOAT_USERNAME
  if [[ $who != did:* ]]; then
    who=$(curl -fsS "https://bsky.social/xrpc/com.atproto.identity.resolveHandle?handle=${who#@}" | jq -r .did)
  fi
  [[ $who == "$DID" ]] || die "$GOAT_USERNAME is $who, but the TXT record points at $DID"
  goat account login
fi

# 3. The record and query lexicons. goat can't read a space type yet, so that
# one is written below as a plain record.
files=()
for f in "$LEXICONS"/*.json; do
  [[ $(basename "$f" .json) == "${SPACE_NSID##*.}" ]] || files+=("$f")
done
say "Publishing ${#files[@]} lexicons"
run goat lex publish --update "${files[@]}"

# 4. The space type declaration.
say "Publishing $SPACE_NSID"
record=$(jq '{"$type": "com.atproto.lexicon.schema"} + .' "$LEXICONS/${SPACE_NSID##*.}.json")
uri="at://$DID/com.atproto.lexicon.schema/$SPACE_NSID"
if ((DRY_RUN)); then
  say "  would write $uri:"
  say "$record" | sed 's/^/    /'
else
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  printf '%s\n' "$record" >"$tmp"
  if goat record get "$uri" >/dev/null 2>&1; then
    goat record update --no-validate --rkey "$SPACE_NSID" "$tmp"
  else
    goat record create --no-validate --rkey "$SPACE_NSID" "$tmp"
  fi
fi

# 5. Read it back the way a PDS would.
if ((DRY_RUN)); then
  say "Would check that $uri can be read back"
else
  say "Checking $uri"
  got=$(goat record get "$uri")
  [[ $(jq -r '.value.defs.main.type // .defs.main.type' <<<"$got") == space ]] || die "$uri didn't come back as a space type"
  say "ok: servers can now find $SPACE_NSID"
fi
