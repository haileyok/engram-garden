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
  if ! out=$(goat account login 2>&1); then
    if grep -qiE "auth.?factor|2fa" <<<"$out"; then
      # The first try made the server email a code.
      say "The server emailed you a sign-in code."
      code=${ATP_AUTH_FACTOR_TOKEN:-}
      [[ -n $code ]] || read -r -p "Code: " code || true
      [[ -n $code ]] || die "no code given"
      goat account login --auth-factor-token "$code" || die "sign-in failed with that code"
    else
      printf '%s\n' "$out" >&2
      die "couldn't sign in"
    fi
  else
    say "$out"
  fi
fi

# After signing in, goat reuses the saved session. With the username and
# password still in the environment, some of its commands sign in again from
# scratch, without the code, so the commands below don't get them.
goat_saved() {
  env -u GOAT_USERNAME -u GOAT_PASSWORD -u ATP_USERNAME -u ATP_PASSWORD \
    -u ATP_AUTH_USERNAME -u ATP_AUTH_PASSWORD -u ATP_AUTH_FACTOR_TOKEN goat "$@"
}

# 3. The record and query lexicons. goat can't read a space type yet, so that
# one is written below as a plain record.
files=()
for f in "$LEXICONS"/*.json; do
  [[ $(basename "$f" .json) == "${SPACE_NSID##*.}" ]] || files+=("$f")
done
say "Publishing ${#files[@]} lexicons"
# goat checks the DNS with this machine's resolver, which may still remember an
# earlier "not found" and make it skip every lexicon without an error. The
# record was checked against the domain's nameserver above.
run goat_saved lex publish --skip-dns-check --update "${files[@]}"

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
  if goat_saved record get "$uri" >/dev/null 2>&1; then
    goat_saved record update --no-validate --rkey "$SPACE_NSID" "$tmp"
  else
    goat_saved record create --no-validate --rkey "$SPACE_NSID" "$tmp"
  fi
fi

# 5. Read every lexicon back the way a PDS would, so a lexicon that was
# skipped doesn't go unnoticed.
if ((DRY_RUN)); then
  say "Would check that every lexicon can be read back from $DID"
else
  say "Checking that every lexicon can be read back"
  missing=()
  for f in "$LEXICONS"/*.json; do
    nsid=$(jq -r .id "$f")
    goat_saved record get "at://$DID/com.atproto.lexicon.schema/$nsid" >/dev/null 2>&1 || missing+=("$nsid")
  done
  ((${#missing[@]} == 0)) || die "not published: ${missing[*]}"
  got=$(goat_saved record get "$uri")
  [[ $(jq -r '.value.defs.main.type // .defs.main.type' <<<"$got") == space ]] || die "$uri didn't come back as a space type"
  say "ok: servers can now find every garden.engram lexicon, including $SPACE_NSID"
fi
