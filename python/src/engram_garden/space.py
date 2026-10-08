"""Space primitives the client needs: references, P-256 did:key encoding, and the HTTP message
signatures space requests carry (RFC 9421, as in cocoon's space package)."""

from __future__ import annotations

import base64
import json
import re
from dataclasses import dataclass

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.asymmetric.utils import decode_dss_signature, encode_dss_signature

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

SIG_LABEL = "atproto-space"
SIG_ALG = "ecdsa-p256-sha256"
HEADER_SPACE_AUDIENCE = "Atproto-Space-Audience"

_P256_ORDER = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551


def signature_base(authorization: str, sig_input: str, audience: str, with_audience: bool) -> bytes:
    lines = [f'"authorization": {authorization.strip()}']
    if with_audience:
        lines.append(f'"atproto-space-audience": {audience.strip()}')
    lines.append(f'"@signature-params": {sig_input}')
    return "\n".join(lines).encode()


def _sign(key: ec.EllipticCurvePrivateKey, data: bytes) -> bytes:
    """SHA-256 + ECDSA, as 64 compact bytes (r||s) with a low s, which every verifier accepts."""
    r, s = decode_dss_signature(key.sign(data, ec.ECDSA(hashes.SHA256())))
    if s > _P256_ORDER // 2:
        s = _P256_ORDER - s
    return r.to_bytes(32, "big") + s.to_bytes(32, "big")


def create_space_sig(key: ec.EllipticCurvePrivateKey, authorization: str, audience: str) -> tuple[str, bytes]:
    """Sign the authorization value and, when audience is set, the audience.

    Returns the signature-input inner list and the compact signature."""
    if audience == "":
        sig_input = f'("authorization");keyid="{did_key(key.public_key())}"'
    else:
        sig_input = '("authorization" "atproto-space-audience")'
    return sig_input, _sign(key, signature_base(authorization, sig_input, audience, audience != ""))


def create_space_sig_headers(key: ec.EllipticCurvePrivateKey, authorization: str, audience: str) -> dict[str, str]:
    """The authorization, audience and signature headers for a space request (lowercase names)."""
    sig_input, sig = create_space_sig(key, authorization, audience)
    out = {
        "authorization": authorization,
        "signature-input": f"{SIG_LABEL}={sig_input}",
        "signature": f"{SIG_LABEL}=:{base64.b64encode(sig).decode()}:",
    }
    if audience:
        out["atproto-space-audience"] = audience
    return out


def verify_space_signature(headers: dict[str, str], key_id: str = "") -> str:
    """Verify headers made by create_space_sig_headers; return the did:key that signed.

    key_id is the credential's bound did:key; empty means a delegation exchange, where the signature
    names its own key. Only the shape this library produces is accepted, which is enough for tests
    and for checking interoperability with the Go implementation."""
    h = {k.lower(): v for k, v in headers.items()}
    m_in = re.fullmatch(rf"{SIG_LABEL}=(\((.*?)\)(?:;keyid=\"(.*)\")?)", h["signature-input"])
    m_sig = re.fullmatch(rf"{SIG_LABEL}=:([A-Za-z0-9+/=]+):", h["signature"])
    if not m_in or not m_sig:
        raise ValueError("missing or malformed atproto-space signature")
    sig_input, items, param_kid = m_in.group(1), m_in.group(2), m_in.group(3)
    expected = '"authorization"' if not key_id else '"authorization" "atproto-space-audience"'
    if items != expected:
        raise ValueError("signature must cover exactly the authorization (and audience) in order")
    signing_key = key_id or param_kid
    if not signing_key:
        raise ValueError("signature key must be a P-256 did:key")
    if key_id and param_kid and param_kid != key_id:
        raise ValueError("signature keyid does not match the credential key")
    pub = public_key_from_did_key(signing_key)
    sig = base64.b64decode(m_sig.group(1))
    if len(sig) != 64:
        raise ValueError("invalid HTTP message signature")
    base = signature_base(h["authorization"], sig_input, h.get("atproto-space-audience", ""), bool(key_id))
    try:
        pub.verify(
            encode_dss_signature(int.from_bytes(sig[:32], "big"), int.from_bytes(sig[32:], "big")),
            base,
            ec.ECDSA(hashes.SHA256()),
        )
    except InvalidSignature:
        raise ValueError("invalid HTTP message signature") from None
    return signing_key


def jwt_payload(token: str) -> dict:
    """The (unverified) payload of a JWT."""
    parts = token.split(".")
    if len(parts) != 3:
        raise ValueError("not a JWT")
    seg = parts[1]
    return json.loads(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)))
