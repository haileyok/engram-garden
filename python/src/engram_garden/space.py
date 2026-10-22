"""Space primitives the client needs: references, P-256 did:key encoding, the HTTP message signatures
space requests carry (RFC 9421) and the MACs a repo host authenticates its responses with, as in
cocoon's space package (the October 22, 2026 alpha)."""

from __future__ import annotations

import base64
import hashlib
import json
import re
from dataclasses import dataclass

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.asymmetric.utils import decode_dss_signature, encode_dss_signature
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

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
SIG_LABEL = "atproto-space-credential"
SIG_TAG = "atproto-space-credential"
SIG_ALG = "ecdsa-p256-sha256"
HEADER_SPACE_AUDIENCE = "Atproto-Space-Audience"
#: What a credential use covers, in order.
_COVERED = '("authorization" "atproto-space-audience")'

# ---- repo MACs ----

COMMIT_MAC_TYPE = "atproto-space-repo"
RECORD_MAC_TYPE = "atproto-space-record"
RECORD_MAC_VERSION = 1
#: The request header asking for a MAC, and the response header carrying it.
HEADER_WANT_MAC = "Atproto-Space-Want-Mac"
HEADER_MAC = "Atproto-Space-Mac"

_P256_ORDER = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551


def signature_base(authorization: str, sig_input: str, audience: str) -> bytes:
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
    credential is bound to (its cnf.kid). Only the shape this library produces is accepted, which is
    enough for tests and for checking interoperability with the Go implementation."""
    h = {k.lower(): v for k, v in headers.items()}
    m_in = re.fullmatch(rf'{SIG_LABEL}=({re.escape(_COVERED)};tag="{SIG_TAG}")', h.get("signature-input", ""))
    m_sig = re.fullmatch(rf"{SIG_LABEL}=:([A-Za-z0-9+/=]+):", h.get("signature", ""))
    if not m_in or not m_sig:
        raise ValueError(f"missing or malformed {SIG_LABEL} signature")
    sig_input = m_in.group(1)
    audience = h.get("atproto-space-audience", "")
    if not audience:
        raise ValueError("missing atproto-space-audience")
    if not key_id:
        raise ValueError("signature key must be a P-256 did:key")
    pub = public_key_from_did_key(key_id)
    sig = base64.b64decode(m_sig.group(1))
    if len(sig) != 64:
        raise ValueError("invalid HTTP message signature")
    base = signature_base(h["authorization"], sig_input, audience)
    try:
        pub.verify(
            encode_dss_signature(int.from_bytes(sig[:32], "big"), int.from_bytes(sig[32:], "big")),
            base,
            ec.ECDSA(hashes.SHA256()),
        )
    except InvalidSignature:
        raise ValueError("invalid HTTP message signature") from None


# ---- MACs: a shared secret per request, and an HMAC over what the host served ----

#: "HPKE-v1" || "KEM" || uint16be(0x0010), naming DHKEM(P-256, HKDF-SHA256).
_KEM_PREFIX = b"HPKE-v1KEM\x00\x10"


def encapsulate(kem_did_key: str) -> tuple[str, bytes]:
    """A syncer's side of one request: the ephemeral key share to send and the shared secret it
    establishes with the repo author's #atproto_kem key. Use a new one for every request."""
    author = public_key_from_did_key(kem_did_key)
    eph = ec.generate_private_key(ec.SECP256R1())
    dh = eph.exchange(ec.ECDH(), author)
    ours = _uncompressed(eph.public_key())
    secret = _derive_shared_secret(dh, ours, _uncompressed(author))
    return did_key(eph.public_key()), secret


def decapsulate(kem: ec.EllipticCurvePrivateKey, key_share: str) -> bytes:
    """The repo host's side: derive the shared secret for a syncer's key_share using the author's KEM
    private key."""
    syncer = public_key_from_did_key(key_share)
    dh = kem.exchange(ec.ECDH(), syncer)
    return _derive_shared_secret(dh, _uncompressed(syncer), _uncompressed(kem.public_key()))


def _uncompressed(pub: ec.EllipticCurvePublicKey) -> bytes:
    return pub.public_bytes(serialization.Encoding.X962, serialization.PublicFormat.UncompressedPoint)


def _derive_shared_secret(dh: bytes, syncer_pub: bytes, author_pub: bytes) -> bytes:
    """DHKEM(P-256, HKDF-SHA256) from RFC 9180 §4.1, as the space alpha derives a MAC's key."""
    input_key = _KEM_PREFIX + b"eae_prk" + dh
    info = b"\x00\x20" + _KEM_PREFIX + b"shared_secret" + syncer_pub + author_pub
    return HKDF(algorithm=hashes.SHA256(), length=32, salt=None, info=info).derive(input_key)


def _mac_version(mac_type: str) -> int:
    return 1


def format_want_mac_header(mac_type: str, key_share: str) -> str:
    """The Atproto-Space-Want-Mac request header, e.g.
    type="atproto-space-repo", ver=1, key_share="did:key:zDna..."."""
    return f'type="{mac_type}", ver={_mac_version(mac_type)}, key_share="{key_share}"'


def parse_mac_header(values: list[str], mac_type: str) -> bytes:
    """The MAC a response carried, from every field line of Atproto-Space-Mac."""
    fields = _parse_mac_dictionary(values, mac_type)
    m = re.fullmatch(r":([A-Za-z0-9+/=]+):", fields.get("mac", ""))
    if not m:
        raise ValueError("missing or malformed mac")
    return base64.b64decode(m.group(1))


def _parse_mac_dictionary(values: list[str], mac_type: str) -> dict[str, str]:
    if not values:
        raise ValueError("missing or malformed MAC header")
    fields: dict[str, str] = {}
    for member in ", ".join(values).split(","):
        key, _, value = member.strip().partition("=")
        fields[key] = value.strip()
    if fields.get("type") != f'"{mac_type}"':
        raise ValueError(f"unsupported MAC type, expected {mac_type}")
    if fields.get("ver") != str(_mac_version(mac_type)):
        raise ValueError(f"unsupported MAC version for {mac_type}")
    return fields


def _length_prefixed(s: str) -> bytes:
    b = s.encode()
    return len(b).to_bytes(2, "big") + b


def compute_record_mac(secret: bytes, uri: str, cid: bytes) -> bytes:
    """HMAC-SHA256 over the record's URI and CID, keyed by the request's shared secret."""
    import hmac

    return hmac.new(
        secret, f"{RECORD_MAC_TYPE}-v{RECORD_MAC_VERSION}".encode() + _length_prefixed(uri) + cid, hashlib.sha256
    ).digest()


def verify_record_mac(secret: bytes, uri: str, cid: bytes, mac: bytes) -> bool:
    import hmac

    return hmac.compare_digest(compute_record_mac(secret, uri, cid), mac)


def cid_bytes(c: str) -> bytes:
    """A CID string's binary form, as a MAC covers it."""
    if not c.startswith("b"):
        raise ValueError(f"not a base32 CID: {c}")
    body = c[1:].upper()
    return base64.b32decode(body + "=" * (-len(body) % 8))


def verify_record(want_uri: str, response: dict, secret: bytes, mac: bytes) -> bool:
    """Check a getRecord response against the MAC it carried: it must be the record requested and
    the MAC must cover its URI and CID. (Go also re-encodes the value to compare with the CID; this
    client has no DRISL encoder.)"""
    if response.get("uri") != want_uri:
        return False
    try:
        cid = cid_bytes(response.get("cid") or "")
    except ValueError:
        return False
    return verify_record_mac(secret, want_uri, cid, mac)


def jwt_payload(token: str) -> dict:
    """The (unverified) payload of a JWT."""
    parts = token.split(".")
    if len(parts) != 3:
        raise ValueError("not a JWT")
    seg = parts[1]
    return json.loads(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)))
