"""Vector helpers: half-precision encoding for vectors carried in records, and normalization."""

from __future__ import annotations

import math
import struct
from collections.abc import Sequence

#: The only vector encoding records may use: little-endian IEEE half precision.
ENCODING_F16LE = "f16le"


def encode_f16(v: Sequence[float]) -> bytes:
    """Encode v as little-endian half precision, 2 bytes per entry."""
    try:
        return struct.pack(f"<{len(v)}e", *v)
    except OverflowError:
        # A value past half precision's range becomes infinity, as in the Go library.
        return b"".join(_f16(x) for x in v)


def _f16(x: float) -> bytes:
    try:
        return struct.pack("<e", x)
    except OverflowError:
        return struct.pack("<e", math.copysign(math.inf, x))


def decode_f16(b: bytes) -> list[float]:
    """Decode little-endian half precision, rejecting odd lengths and non-finite values."""
    if len(b) % 2:
        raise ValueError("f16 vector has an odd number of bytes")
    out = list(struct.unpack(f"<{len(b) // 2}e", b))
    for i, x in enumerate(out):
        if not math.isfinite(x):
            raise ValueError(f"f16 vector entry {i} is not finite")
    return out


def normalize(v: list[float]) -> bool:
    """Scale v to unit length in place. False for a zero vector, which has no direction."""
    total = math.fsum(x * x for x in v)
    if total == 0 or not math.isfinite(total):
        return False
    n = 1.0 / math.sqrt(total)
    for i in range(len(v)):
        v[i] *= n
    return True
