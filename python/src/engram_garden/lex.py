"""Record formats: the space's declared embedding model (garden.engram.config) and the vectors
memories carry (garden.engram.memory's embedding fields). Mirrors internal/lex."""

from __future__ import annotations

import base64
import binascii
from dataclasses import dataclass, field, replace
from datetime import UTC, datetime
from typing import Any

from . import vec

SPACE_TYPE = "garden.engram.space"
MEMORY_COLLECTION = "garden.engram.memory"
CONFIG_COLLECTION = "garden.engram.config"
#: The config record's key in the authority's repo.
CONFIG_RKEY = "self"
#: Bounds the text sent to an embedding model, to stay inside common models' input limits.
MAX_EMBED_CHARS = 24000

#: Memory record fields that may hold vectors.
MEMORY_EMBEDDING_FIELDS = ("embedding", "nextEmbedding")


@dataclass(frozen=True)
class ModelInfo:
    """Identifies an embedding model exactly. Vectors are comparable only when all three match."""

    model: str = ""
    model_digest: str = ""
    dims: int = 0

    def key(self) -> str:
        return f"{self.model}@{self.model_digest}/{self.dims}"

    def __str__(self) -> str:
        return f"{self.model} ({self.model_digest}, {self.dims} dims)"

    def valid(self) -> bool:
        return bool(self.model) and bool(self.model_digest) and 0 < self.dims <= 16000

    def to_dict(self) -> dict[str, Any]:
        return {"model": self.model, "modelDigest": self.model_digest, "dims": self.dims}


@dataclass(frozen=True)
class Config:
    """The space authority's garden.engram.config record."""

    model_info: ModelInfo = field(default_factory=ModelInfo)
    #: Goes before stored text; query_prefix before queries (for models trained with task prefixes).
    document_prefix: str = ""
    query_prefix: str = ""
    #: The model a change is moving to. Agents embed with both while it's set.
    next: ModelInfo | None = None

    @property
    def model(self) -> str:
        return self.model_info.model

    @property
    def model_digest(self) -> str:
        return self.model_info.model_digest

    @property
    def dims(self) -> int:
        return self.model_info.dims

    def record(self, created_at: datetime | None = None) -> dict[str, Any]:
        """The config as a record value."""
        rec: dict[str, Any] = self.model_info.to_dict()
        rec["$type"] = CONFIG_COLLECTION
        rec["createdAt"] = format_time(created_at or datetime.now(UTC))
        if self.document_prefix:
            rec["documentPrefix"] = self.document_prefix
        if self.query_prefix:
            rec["queryPrefix"] = self.query_prefix
        if self.next is not None:
            rec["next"] = self.next.to_dict()
        return rec


def format_time(t: datetime) -> str:
    """RFC 3339 with milliseconds and a Z, as the Go library writes createdAt."""
    t = t.astimezone(UTC)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + f"{t.microsecond // 1000:03d}Z"


def _int_of(v: Any) -> int:
    if isinstance(v, bool):
        return 0
    if isinstance(v, (int, float)):
        return int(v)
    return 0


def parse_model(rec: dict[str, Any]) -> ModelInfo:
    m = ModelInfo(
        model=rec.get("model") if isinstance(rec.get("model"), str) else "",
        model_digest=rec.get("modelDigest") if isinstance(rec.get("modelDigest"), str) else "",
        dims=_int_of(rec.get("dims")),
    )
    if not m.valid():
        raise ValueError("model, modelDigest and dims (1-16000) are required")
    return m


def parse_config(rec: dict[str, Any]) -> Config:
    """Read a garden.engram.config record."""
    try:
        m = parse_model(rec)
    except ValueError as e:
        raise ValueError(f"config: {e}") from None
    nxt = None
    n = rec.get("next")
    if isinstance(n, dict):
        try:
            nm = parse_model(n)
        except ValueError as e:
            raise ValueError(f"config next: {e}") from None
        if nm != m:
            nxt = nm
    dp, qp = rec.get("documentPrefix"), rec.get("queryPrefix")
    return Config(
        model_info=m,
        document_prefix=dp if isinstance(dp, str) else "",
        query_prefix=qp if isinstance(qp, str) else "",
        next=nxt,
    )


@dataclass(frozen=True)
class Embedding:
    model_info: ModelInfo
    vector: list[float]

    def key(self) -> str:
        return self.model_info.key()


def embedding_record(m: ModelInfo, v: list[float]) -> dict[str, Any]:
    """Encode a vector for a memory record's embedding field."""
    rec = m.to_dict()
    rec["encoding"] = vec.ENCODING_F16LE
    # The atproto data model encodes $bytes as unpadded base64.
    rec["vector"] = {"$bytes": base64.b64encode(vec.encode_f16(v)).decode().rstrip("=")}
    return rec


def _bytes_of(v: Any) -> bytes | None:
    if isinstance(v, (bytes, bytearray)):
        return bytes(v)
    if isinstance(v, dict) and isinstance(v.get("$bytes"), str):
        s = v["$bytes"].rstrip("=")
        try:
            return base64.b64decode(s + "=" * (-len(s) % 4))
        except (binascii.Error, ValueError):
            return None
    return None


def parse_embeddings(rec: dict[str, Any]) -> list[Embedding]:
    """Read a memory record's vectors. Malformed entries are skipped."""
    out: list[Embedding] = []
    for f in MEMORY_EMBEDDING_FIELDS:
        e = rec.get(f)
        if not isinstance(e, dict):
            continue
        try:
            m = parse_model(e)
        except ValueError:
            continue
        if e.get("encoding") != vec.ENCODING_F16LE:
            continue
        raw = _bytes_of(e.get("vector"))
        if raw is None:
            continue
        try:
            v = vec.decode_f16(raw)
        except ValueError:
            continue
        if len(v) != m.dims:
            continue
        out.append(Embedding(m, v))
    return out


def encode_query_vector(v: list[float]) -> str:
    """For searchMemories' vector parameter: base64url (unpadded) little-endian half precision."""
    return base64.urlsafe_b64encode(vec.encode_f16(v)).decode().rstrip("=")


def decode_query_vector(s: str) -> list[float]:
    s = s.rstrip("=")
    try:
        raw = base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))
    except (binascii.Error, ValueError):
        raise ValueError("vector must be base64url-encoded f16le") from None
    return vec.decode_f16(raw)


def embed_text(prefix: str, text: str, tags: list[str] | None = None) -> str:
    """What gets embedded for a memory: its text, plus its tags so they help retrieval.

    The space's document prefix goes in front."""
    raw = text.encode()
    if len(raw) > MAX_EMBED_CHARS:
        text = raw[:MAX_EMBED_CHARS].decode(errors="ignore")
    if tags:
        text += "\n\nTags: " + ", ".join(tags)
    return prefix + text


# ---- changing the space's model (the authority's side) ----

DECLARE = "declare"
START_NEXT = "next"
PROMOTE = "promote"
CANCEL_NEXT = "cancel"


def default_prefixes(model: str, document_prefix: str, query_prefix: str) -> tuple[str, str]:
    """Fill in a model's known task prefixes unless given."""
    if not document_prefix and not query_prefix and model.startswith("nomic-embed-text"):
        return "search_document: ", "search_query: "
    return document_prefix, query_prefix


def change_config(
    cur: Config | None,
    action: str,
    m: ModelInfo | None = None,
    document_prefix: str = "",
    query_prefix: str = "",
) -> Config:
    """Apply an action to the current config (None when the space has none)."""
    m = m or ModelInfo()
    if action == DECLARE:
        if not m.valid():
            raise ValueError("the model needs a name, a digest and dimensions")
        dp, qp = default_prefixes(m.model, document_prefix, query_prefix)
        return Config(model_info=m, document_prefix=dp, query_prefix=qp)
    if action == START_NEXT:
        if cur is None:
            raise ValueError("declare a model first")
        if not m.valid():
            raise ValueError("the next model needs a name, a digest and dimensions")
        if m.key() == cur.model_info.key():
            raise ValueError("that's already the space's model")
        return replace(cur, next=m)
    if action == PROMOTE:
        if cur is None or cur.next is None:
            raise ValueError("no model change in progress")
        dp, qp = default_prefixes(cur.next.model, document_prefix, query_prefix)
        return Config(model_info=cur.next, document_prefix=dp, query_prefix=qp)
    if action == CANCEL_NEXT:
        if cur is None:
            raise ValueError("the space has no model")
        return replace(cur, next=None)
    raise ValueError("unknown action: use declare, next, promote or cancel")
