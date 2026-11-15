"""Space primitives the client needs: references, P-256 did:key encoding, the HTTP message signatures
space requests carry (RFC 9421) and the MACs a repo host authenticates its responses with, as in
cocoon's space package (the November 12, 2026 alpha)."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import re
from dataclasses import dataclass

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.asymmetric.utils import decode_dss_signature, encode_dss_signature
from cryptography.hazmat.primitives.kdf.hkdf import HKDF, HKDFExpand

# ---- references ----

_DID_RE = re.compile(r"^did:[a-z]+:[a-zA-Z0-9._:%-]*[a-zA-Z0-9._-]$")
_NSID_RE = re.compile(
    r"^[a-zA-Z](?:[a-zA-Z0-9-]{0,62}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,62}[a-zA-Z0-9])?)+"
    r"\.[a-zA-Z][a-zA-Z0-9]{0,62}$"
)
_RKEY_RE = re.compile(r"^[a-zA-Z0-9._:~-]{1,512}$")


def valid_did(s: str) -> bool:
    return len(s) <= 2048 and bool(_DID_RE.match(s))


def valid_record_key(s: str) -> bool:
    return s not in (".", "..") and bool(_RKEY_RE.match(s))


@dataclass(frozen=True)
class Ref:
    """Names a space: at://{authority}/space/{spaceType}/{skey}."""

    authority: str
    type: str
    skey: str

    @classmethod
    def parse(cls, s: str) -> Ref:
        rest = s.removeprefix("at://") if s.startswith("at://") else None
        if rest is None:
            raise ValueError(f"not a space uri: {s}")
        parts = rest.split("/")
        if len(parts) != 4 or parts[1] != "space":
            raise ValueError(f"not a space uri: {s}")
        ref = cls(authority=parts[0], type=parts[2], skey=parts[3])
        if not valid_did(ref.authority) or not _NSID_RE.match(ref.type) or not valid_record_key(ref.skey):
            raise ValueError(f"not a space uri: {s}")
        return ref

    def __str__(self) -> str:
        return f"at://{self.authority}/space/{self.type}/{self.skey}"

    def record_uri(self, author: str, collection: str, rkey: str) -> str:
        return f"{self}/{author}/{collection}/{rkey}"


# ---- P-256 keys as did:key ----

_B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
_P256_MULTICODEC = b"\x80\x24"  # varint(0x1200)


def _b58encode(b: bytes) -> str:
    n = int.from_bytes(b, "big")
    out = ""
    while n:
        n, r = divmod(n, 58)
        out = _B58[r] + out
    pad = len(b) - len(b.lstrip(b"\0"))
    return "1" * pad + out


def _b58decode(s: str) -> bytes:
    n = 0
    for c in s:
        n = n * 58 + _B58.index(c)
    body = n.to_bytes((n.bit_length() + 7) // 8, "big")
    pad = len(s) - len(s.lstrip("1"))
    return b"\0" * pad + body


def generate_key() -> ec.EllipticCurvePrivateKey:
    return ec.generate_private_key(ec.SECP256R1())


def did_key(pub: ec.EllipticCurvePublicKey) -> str:
    """The did:key of a P-256 public key (multicodec 0x1200, compressed point, base58btc)."""
    point = pub.public_bytes(serialization.Encoding.X962, serialization.PublicFormat.CompressedPoint)
    return "did:key:z" + _b58encode(_P256_MULTICODEC + point)


def public_key_from_did_key(s: str) -> ec.EllipticCurvePublicKey:
    if not s.startswith("did:key:z"):
        raise ValueError("signature key must be a P-256 did:key")
    raw = _b58decode(s.removeprefix("did:key:z"))
    if not raw.startswith(_P256_MULTICODEC):
        raise ValueError("signature key must be a P-256 did:key")
    return ec.EllipticCurvePublicKey.from_encoded_point(ec.SECP256R1(), raw[len(_P256_MULTICODEC) :])


# ---- HTTP message signatures ----

#: The Authorization scheme a space credential is presented with.
CREDENTIAL_SCHEME = "Atproto-Space-Credential"
#: The label signers use. Labels carry no meaning (RFC 9421 §7.2.5): verifiers find the signature by
#: its tag.
SIG_LABEL = "sig"
SIG_TAG = "atproto-space-credential"
SIG_ALG = "ecdsa-p256-sha256"
HEADER_SPACE_AUDIENCE = "Atproto-Space-Audience"
#: What a credential use covers, in order.
_COVERED = '("authorization" "atproto-space-audience")'

# ---- repo MACs ----

#: A MAC is an HTTP message signature (hmac-sha256) over the response header its profile names. The
#: syncer requests one with an Accept-Signature header whose tag is the profile.
COMMIT_MAC_PROFILE = "atproto-space-commit"
RECORD_MAC_PROFILE = "atproto-space-record"
HEADER_ACCEPT_SIGNATURE = "Accept-Signature"
HEADER_COMMIT = "Atproto-Space-Commit"
HEADER_RECORD = "Atproto-Space-Record"
MAC_ALG = "hmac-sha256"
#: The label a syncer requests its MAC under.
MAC_LABEL = "mac"

_P256_ORDER = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551


def signature_base(authorization: str, sig_input: str, audience: str) -> bytes:
    """The RFC 9421 signature base for a credential use; sig_input is the signature-input inner list
    with its parameters."""
    lines = [f'"authorization": {authorization.strip()}', f'"atproto-space-audience": {audience.strip()}']
    lines.append(f'"@signature-params": {sig_input}')
    return "\n".join(lines).encode()


def _sign(key: ec.EllipticCurvePrivateKey, data: bytes) -> bytes:
    """SHA-256 + ECDSA, as 64 compact bytes (r||s) with a low s, which every verifier accepts."""
    r, s = decode_dss_signature(key.sign(data, ec.ECDSA(hashes.SHA256())))
    if s > _P256_ORDER // 2:
        s = _P256_ORDER - s
    return r.to_bytes(32, "big") + s.to_bytes(32, "big")


def create_space_sig(key: ec.EllipticCurvePrivateKey, authorization: str, audience: str) -> tuple[str, bytes]:
    """Sign a request authorized by a space credential: the credential (which must use the
    Atproto-Space-Credential scheme) and its audience DID. Returns the signature-input inner list and
    the compact signature."""
    if not audience:
        raise ValueError("a space credential signature requires an audience")
    sig_input = f'{_COVERED};tag="{SIG_TAG}"'
    return sig_input, _sign(key, signature_base(authorization, sig_input, audience))


def create_space_sig_headers(key: ec.EllipticCurvePrivateKey, authorization: str, audience: str) -> dict[str, str]:
    """The authorization, audience and signature headers for a space request (lowercase names)."""
    sig_input, sig = create_space_sig(key, authorization, audience)
    return {
        "authorization": authorization,
        "atproto-space-audience": audience,
        "signature-input": f"{SIG_LABEL}={sig_input}",
        "signature": f"{SIG_LABEL}=:{base64.b64encode(sig).decode()}:",
    }


def verify_credential_sig(headers: dict[str, str], key_id: str) -> None:
    """Verify the signature on a request authorized by a space credential, against the key the
    credential is bound to (its cnf.kid). The signature is found by its tag, under any label. Only the
    shape this library produces is accepted, which is enough for tests and for checking
    interoperability with the Go implementation."""
    h = {k.lower(): v for k, v in headers.items()}
    found = _find_signature(
        [h.get("signature-input", "")],
        [h.get("signature", "")],
        tag=SIG_TAG,
        components=_COVERED,
        alg=SIG_ALG,
        key_id=key_id,
    )
    audience = h.get("atproto-space-audience", "")
    if not audience:
        raise ValueError("missing atproto-space-audience")
    if not key_id:
        raise ValueError("signature key must be a P-256 did:key")
    pub = public_key_from_did_key(key_id)
    sig = found.signature
    if len(sig) != 64:
        raise ValueError("invalid HTTP message signature")
    base = signature_base(h["authorization"], found.params, audience)
    try:
        pub.verify(
            encode_dss_signature(int.from_bytes(sig[:32], "big"), int.from_bytes(sig[32:], "big")),
            base,
            ec.ECDSA(hashes.SHA256()),
        )
    except InvalidSignature:
        raise ValueError("invalid HTTP message signature") from None


@dataclass
class _Found:
    params: str  # the signature-input inner list with its parameters, as sent
    signature: bytes


def _split_top(s: str, sep: str) -> list[str]:
    """Split on a separator outside quotes and parentheses."""
    out, depth, quoted, cur, i = [], 0, False, "", 0
    while i < len(s):
        c = s[i]
        if quoted:
            cur += c
            if c == "\\" and i + 1 < len(s):
                i += 1
                cur += s[i]
            elif c == '"':
                quoted = False
        elif c == '"':
            quoted = True
            cur += c
        elif c == "(":
            depth += 1
            cur += c
        elif c == ")":
            depth -= 1
            cur += c
        elif c == sep and depth == 0:
            out.append(cur.strip())
            cur = ""
        else:
            cur += c
        i += 1
    if cur.strip():
        out.append(cur.strip())
    return out


def _inner_list(member: str) -> tuple[str, dict[str, str]] | None:
    """An inner list member's covered components text and its parameters (values as serialized)."""
    m = re.fullmatch(r"\(([^)]*)\)((?:;.*)?)", member)
    if not m:
        return None
    params: dict[str, str] = {}
    for p in _split_top(m.group(2), ";"):
        if not p:
            continue
        key, _, value = p.partition("=")
        params[key] = value if value else "?1"
    return m.group(1), params


def _unquote(v: str) -> str | None:
    if len(v) >= 2 and v[0] == v[-1] == '"':
        return re.sub(r"\\(.)", r"\1", v[1:-1])
    return None


def _find_signature(inputs: list[str], sigs: list[str], *, tag: str, components: str, alg: str, key_id: str) -> _Found:
    """The one signature tagged `tag` in a Signature-Input / Signature pair, which must cover exactly
    `components` and carry no other algorithm or key than the ones expected (RFC 9421 §3.2.1)."""
    err = f'missing or malformed signature with tag "{tag}"'
    members = _split_top(", ".join(inputs), ",")
    tagged = []
    for member in members:
        label, _, rest = member.partition("=")
        parsed = _inner_list(rest)
        if parsed and _unquote(parsed[1].get("tag", "")) == tag:
            tagged.append((label, rest, parsed))
    if not tagged:
        raise ValueError(err)
    if len(tagged) > 1:
        raise ValueError(f'more than one signature with tag "{tag}"')
    label, rest, (comps, params) = tagged[0]
    if re.sub(r"\s+", " ", comps) != re.sub(r"\s+", " ", components.strip("()")):
        raise ValueError(f"signature must cover exactly {components}")
    if "alg" in params and _unquote(params["alg"]) != alg:
        raise ValueError(f"signature algorithm must be {alg}")
    if "keyid" in params and _unquote(params["keyid"]) != key_id:
        raise ValueError("signature keyid does not match the expected key")
    raw = None
    for member in _split_top(", ".join(sigs), ","):
        sl, _, sv = member.partition("=")
        if sl == label:
            raw = sv
    m = re.fullmatch(r":([A-Za-z0-9+/=]+):", raw or "")
    if not m:
        raise ValueError(err)
    return _Found(params=rest, signature=base64.b64decode(m.group(1)))


# ---- MACs: a shared secret per request, and an HMAC signature over what the host served ----

#: "HPKE-v1" || "KEM" || uint16be(0x0010), naming DHKEM(P-256, HKDF-SHA256).
_KEM_PREFIX = b"HPKE-v1KEM\x00\x10"
#: The HPKE suite: DHKEM(P-256, HKDF-SHA256) 0x0010, HKDF-SHA256 0x0001, export-only AEAD 0xFFFF.
_HPKE_SUITE = b"HPKE\x00\x10\x00\x01\xff\xff"
_HPKE_INFO = b"atproto-mac"
_HPKE_SECRET_LEN = 32


@dataclass(frozen=True)
class Encapsulation:
    """A syncer's side of one request: the ephemeral public key (HPKE's enc) as a did:key, sent as
    the keyid of its MAC request, and the secret it shares with the repo host."""

    key_share: str
    shared_secret: bytes


def encapsulate(kem_did_key: str) -> Encapsulation:
    """Generate a fresh ephemeral key and derive the shared secret against the repo author's
    #atproto_kem key, as HPKE's secret export does (RFC 9180 §6.2: base mode, DHKEM(P-256,
    HKDF-SHA256), HKDF-SHA256, export-only AEAD, info "atproto-mac", empty exporter context). Use a
    new one for every request."""
    author = public_key_from_did_key(kem_did_key)
    eph = ec.generate_private_key(ec.SECP256R1())
    dh = eph.exchange(ec.ECDH(), author)
    shared = _derive_shared_secret(dh, _uncompressed(eph.public_key()), _uncompressed(author))
    return Encapsulation(did_key(eph.public_key()), _export_secret(shared))


def decapsulate(kem: ec.EllipticCurvePrivateKey, key_share: str) -> bytes:
    """The repo host's side: derive the shared secret for a syncer's key share using the author's KEM
    private key."""
    syncer = public_key_from_did_key(key_share)
    dh = kem.exchange(ec.ECDH(), syncer)
    return _export_secret(_derive_shared_secret(dh, _uncompressed(syncer), _uncompressed(kem.public_key())))


def _uncompressed(pub: ec.EllipticCurvePublicKey) -> bytes:
    return pub.public_bytes(serialization.Encoding.X962, serialization.PublicFormat.UncompressedPoint)


def _derive_shared_secret(dh: bytes, enc: bytes, author_pub: bytes) -> bytes:
    """DHKEM(P-256, HKDF-SHA256) from RFC 9180 §4.1: the KEM's shared secret."""
    input_key = _KEM_PREFIX + b"eae_prk" + dh
    info = b"\x00\x20" + _KEM_PREFIX + b"shared_secret" + enc + author_pub
    return HKDF(algorithm=hashes.SHA256(), length=32, salt=None, info=info).derive(input_key)


def _labeled_extract(salt: bytes, label: bytes, ikm: bytes) -> bytes:
    return hmac.new(salt or bytes(32), b"HPKE-v1" + _HPKE_SUITE + label + ikm, hashlib.sha256).digest()


def _labeled_expand(prk: bytes, label: bytes, info: bytes, length: int) -> bytes:
    labeled = length.to_bytes(2, "big") + b"HPKE-v1" + _HPKE_SUITE + label + info
    return HKDFExpand(algorithm=hashes.SHA256(), length=length, info=labeled).derive(prk)


def _export_secret(shared_secret: bytes) -> bytes:
    """HPKE's key schedule (RFC 9180 §5.1, base mode, no PSK) and Export("", 32)."""
    psk_id_hash = _labeled_extract(b"", b"psk_id_hash", b"")
    info_hash = _labeled_extract(b"", b"info_hash", _HPKE_INFO)
    context = b"\x00" + psk_id_hash + info_hash
    secret = _labeled_extract(shared_secret, b"secret", b"")
    exporter_secret = _labeled_expand(secret, b"exp", context, 32)
    return _labeled_expand(exporter_secret, b"sec", b"", _HPKE_SECRET_LEN)


def _covered_components(profile: str) -> str:
    return f'("{profile_header(profile).lower()}";sf)'


def profile_header(profile: str) -> str:
    """The response header a profile's MAC covers."""
    return HEADER_COMMIT if profile == COMMIT_MAC_PROFILE else HEADER_RECORD


def format_mac_request(profile: str, key_share: str) -> str:
    """The Accept-Signature request header asking for a MAC under a profile, keyed by the shared
    secret for key_share, e.g.
    mac=("atproto-space-record";sf);keyid="did:key:zDna...";tag="atproto-space-record"."""
    return f'{MAC_LABEL}={_covered_components(profile)};keyid="{key_share}";tag="{profile}"'


@dataclass(frozen=True)
class MacRequest:
    """A MAC request as the host reads it from Accept-Signature."""

    label: str
    key_share: str
    #: The requested signature parameters, serialized; the host's MAC uses them as given.
    params: str


def parse_mac_request(values: list[str], profile: str) -> MacRequest | None:
    """The MAC requested for profile in the Accept-Signature field lines, or None if there is none.
    Other tags are ignored."""
    if not values:
        return None
    tagged = []
    for member in _split_top(", ".join(values), ","):
        label, _, rest = member.partition("=")
        parsed = _inner_list(rest)
        if parsed and _unquote(parsed[1].get("tag", "")) == profile:
            tagged.append((label, rest, parsed))
    if not tagged:
        return None
    if len(tagged) > 1:
        raise ValueError(f'more than one MAC requested with tag "{profile}"')
    label, rest, (comps, params) = tagged[0]
    if comps != _covered_components(profile).strip("()"):
        raise ValueError(f"MAC must cover exactly {_covered_components(profile)}")
    for key in params:
        if key not in ("keyid", "tag", "alg"):
            raise ValueError(f"unsupported MAC parameter: {key}")
    if "alg" in params and _unquote(params["alg"]) != MAC_ALG:
        raise ValueError(f"MAC algorithm must be {MAC_ALG}")
    keyid = _unquote(params.get("keyid", ""))
    if not keyid or not valid_did(keyid):
        raise ValueError("missing or malformed keyid")
    return MacRequest(label=label, key_share=keyid, params=rest)


def _sf_string(v: str) -> str:
    return '"' + v.replace("\\", "\\\\").replace('"', '\\"') + '"'


def _mac_base(profile: str, covered: str, params: str) -> bytes:
    return f'"{profile_header(profile).lower()}";sf: {covered}\n"@signature-params": {params}'.encode()


def create_record_mac_headers(uri: str, cid: str, request: MacRequest, secret: bytes) -> dict[str, str]:
    """The response headers for a record: an Atproto-Space-Record header naming it, and a MAC covering
    that header, keyed by lowercase header name (what a repo host sends; here, for tests)."""
    covered = f"uri={_sf_string(uri)}, cid={_sf_string(cid)}"
    mac = hmac.new(secret, _mac_base(RECORD_MAC_PROFILE, covered, request.params), hashlib.sha256).digest()
    return {
        HEADER_RECORD.lower(): covered,
        "signature-input": f"{request.label}={request.params}",
        "signature": f"{request.label}=:{base64.b64encode(mac).decode()}:",
    }


def verify_record_mac(
    uri: str, cid: str, enc: Encapsulation, signature_input: list[str], signature: list[str], covered: list[str]
) -> None:
    """Verify a response's MAC, and that the Atproto-Space-Record header it covers names uri and cid.
    Raises ValueError otherwise."""
    found = _find_signature(
        signature_input,
        signature,
        tag=RECORD_MAC_PROFILE,
        components=_covered_components(RECORD_MAC_PROFILE),
        alg=MAC_ALG,
        key_id=enc.key_share,
    )
    if not covered:
        raise ValueError(f"missing or malformed {HEADER_RECORD} header")
    fields: dict[str, str] = {}
    for member in _split_top(", ".join(covered), ","):
        key, _, value = member.partition("=")
        v = _unquote(value)
        if v is None:
            raise ValueError(f"malformed {HEADER_RECORD} header")
        fields[key] = v
    # The sf flag covers the header as re-serialized from its parsed form.
    reserialized = ", ".join(f"{k}={_sf_string(v)}" for k, v in fields.items())
    expected = hmac.new(enc.shared_secret, _mac_base(RECORD_MAC_PROFILE, reserialized, found.params), hashlib.sha256)
    if not hmac.compare_digest(expected.digest(), found.signature):
        raise ValueError("MAC does not match")
    if fields.get("uri") != uri or fields.get("cid") != cid:
        raise ValueError("record does not match its MAC")


def verify_record(
    want_uri: str,
    response: dict,
    enc: Encapsulation,
    signature_input: list[str],
    signature: list[str],
    covered: list[str],
) -> None:
    """Check a getRecord response against the MAC it carried: it must be the record requested and the
    MAC must cover its URI and CID. (Go also re-encodes the value to compare with the CID; this client
    has no DRISL encoder.) Raises ValueError if it doesn't."""
    if response.get("uri") != want_uri:
        raise ValueError(f"record is not the one requested: {response.get('uri')}")
    verify_record_mac(want_uri, response.get("cid") or "", enc, signature_input, signature, covered)


def cid_bytes(c: str) -> bytes:
    """A CID string's binary form."""
    if not c.startswith("b"):
        raise ValueError(f"not a base32 CID: {c}")
    body = c[1:].upper()
    return base64.b32decode(body + "=" * (-len(body) % 8))


def jwt_payload(token: str) -> dict:
    """The (unverified) payload of a JWT."""
    parts = token.split(".")
    if len(parts) != 3:
        raise ValueError("not a JWT")
    seg = parts[1]
    return json.loads(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)))
