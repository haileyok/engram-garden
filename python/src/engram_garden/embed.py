"""Turning text into vectors for semantic search. Mirrors internal/embed."""

from __future__ import annotations

import asyncio
import math
import re
from dataclasses import dataclass, field
from typing import Protocol

import httpx

from .errors import EngramError, ModelMismatchError
from .lex import ModelInfo

DEFAULT_BASE_URL = "http://localhost:11434/v1"
#: The digest HashingProvider accepts.
HASHING_DIGEST = "sha256:hashing"


class Embedder(Protocol):
    """Embeds text. Every vector it returns has dimensions() entries."""

    async def embed(self, texts: list[str]) -> list[list[float]]: ...

    def dimensions(self) -> int: ...

    def model(self) -> str: ...


class Provider(Protocol):
    """Returns an embedder for a space's declared model, after checking the local model is that model."""

    async def for_model(self, m: ModelInfo) -> Embedder: ...


@dataclass
class OpenAIEmbedder:
    """Calls an OpenAI-compatible /embeddings endpoint (OpenAI, Ollama, llama.cpp, vLLM)."""

    base_url: str
    name: str
    dims: int
    api_key: str = ""
    #: Ask the model for dims-sized vectors. Only models that support shortening accept it.
    send_dimensions: bool = False
    batch_size: int = 64
    http: httpx.AsyncClient | None = None

    def dimensions(self) -> int:
        return self.dims

    def model(self) -> str:
        return self.name

    async def embed(self, texts: list[str]) -> list[list[float]]:
        out: list[list[float]] = []
        batch = self.batch_size if self.batch_size > 0 else 64
        for start in range(0, len(texts), batch):
            out.extend(await self._embed_batch(texts[start : start + batch]))
        return out

    async def _embed_batch(self, texts: list[str]) -> list[list[float]]:
        req: dict = {"model": self.name, "input": texts}
        if self.send_dimensions:
            req["dimensions"] = self.dims
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = "Bearer " + self.api_key
        client = self.http or httpx.AsyncClient(timeout=60)
        try:
            try:
                resp = await client.post(self.base_url.rstrip("/") + "/embeddings", json=req, headers=headers)
            except httpx.HTTPError as e:
                raise EngramError(f"embeddings request: {e}") from e
        finally:
            if self.http is None:
                await client.aclose()
        if resp.status_code != 200:
            raise EngramError(f"embeddings request: {resp.status_code}: {_truncate(resp.text, 300)}")
        try:
            data = resp.json()["data"]
        except (ValueError, KeyError, TypeError) as e:
            raise EngramError(f"embeddings response: {e}") from e
        if len(data) != len(texts):
            raise EngramError(f"embeddings response: got {len(data)} vectors for {len(texts)} inputs")
        out: list[list[float] | None] = [None] * len(texts)
        for d in data:
            i = d.get("index", -1)
            if not isinstance(i, int) or i < 0 or i >= len(texts) or out[i] is not None:
                raise EngramError("embeddings response: bad or duplicate index")
            v = d["embedding"]
            if self.dims >= 0 and len(v) != self.dims:
                raise EngramError(
                    f"embeddings response: model returned {len(v)} dimensions, configured for {self.dims}"
                )
            out[i] = [float(x) for x in v]
        return [v for v in out if v is not None]


def _truncate(s: str, n: int) -> str:
    return s if len(s) <= n else s[:n] + "…"


async def probe_dims(base_url: str, api_key: str, model: str, http: httpx.AsyncClient | None = None) -> int:
    """Embed one short text and return the vector size the model produces."""
    e = OpenAIEmbedder(base_url=base_url, api_key=api_key, name=model, dims=-1, http=http)
    return len((await e._embed_batch(["dimension probe"]))[0])


@dataclass
class HashingEmbedder:
    """A deterministic, offline embedder: it hashes words into buckets and normalizes, so texts
    sharing words land near each other. It has no understanding of meaning; for tests only."""

    dims: int

    def dimensions(self) -> int:
        return self.dims

    def model(self) -> str:
        return f"hashing-{self.dims}"

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [hash_embed(t, self.dims) for t in texts]


def hash_embed(text: str, dims: int) -> list[float]:
    """The hashing embedder's vector for one text (synchronous, for tests)."""
    v = [0.0] * dims
    for w in re.findall(r"[^\W_]+", text.lower()):
        v[_fnv32a(w.encode()) % dims] += 1
    total = math.fsum(x * x for x in v)
    if total == 0:
        v[0] = 1.0
        return v
    n = math.sqrt(total)
    return [x / n for x in v]


def _fnv32a(b: bytes) -> int:
    h = 0x811C9DC5
    for c in b:
        h = ((h ^ c) * 0x01000193) & 0xFFFFFFFF
    return h


async def ollama_digest(http: httpx.AsyncClient, openai_base: str, name: str) -> str:
    """The digest ("sha256:<hex>") of a local Ollama model, given Ollama's OpenAI-compatible base URL.
    Returns "" if the model isn't installed."""
    root = openai_base.rstrip("/").removesuffix("/v1")
    resp = await http.get(root + "/api/tags")
    if resp.status_code != 200:
        raise EngramError(
            f"{root}/api/tags: {resp.status_code}: {resp.text[:300]} (not Ollama? set ENGRAM_EMBED_MODEL_DIGEST)"
        )
    want = name if ":" in name else name + ":latest"
    for m in resp.json().get("models", []):
        if want in (m.get("name"), m.get("model")) or name in (m.get("name"), m.get("model")):
            d = m.get("digest", "")
            if not d:
                return ""
            return d if d.startswith("sha256:") else "sha256:" + d
    return ""


@dataclass
class OpenAIProvider:
    """Embeds through an OpenAI-compatible endpoint, Ollama by default. It learns the local model's
    digest from Ollama's /api/tags, or takes it from digest for other servers."""

    base_url: str = ""
    api_key: str = ""
    #: Overrides the model name sent to the endpoint, when the local name differs from the declared one.
    model_name: str = ""
    #: Declares the local model's digest, for servers that aren't Ollama.
    digest: str = ""
    http: httpx.AsyncClient | None = None
    _cache: dict[str, Embedder] = field(default_factory=dict)
    _lock: asyncio.Lock = field(default_factory=asyncio.Lock)

    def _base(self) -> str:
        return self.base_url.rstrip("/") if self.base_url else DEFAULT_BASE_URL

    async def for_model(self, m: ModelInfo) -> Embedder:
        async with self._lock:
            if (e := self._cache.get(m.key())) is not None:
                return e
            name = self.model_name or m.model
            digest = self.digest
            if not digest:
                client = self.http or httpx.AsyncClient(timeout=60)
                try:
                    digest = await ollama_digest(client, self._base(), name)
                except httpx.HTTPError as e:
                    raise EngramError(f"checking the local model: {e}") from e
                except EngramError as e:
                    raise EngramError(f"checking the local model: {e}") from e
                finally:
                    if self.http is None:
                        await client.aclose()
            if digest != m.model_digest:
                raise ModelMismatchError(m, name, digest)
            e = OpenAIEmbedder(base_url=self._base(), api_key=self.api_key, name=name, dims=m.dims, http=self.http)
            self._cache[m.key()] = e
            return e


class HashingProvider:
    """Serves the offline HashingEmbedder for any model named "hashing…" with HASHING_DIGEST."""

    async def for_model(self, m: ModelInfo) -> Embedder:
        if not m.model.startswith("hashing") or m.model_digest != HASHING_DIGEST:
            raise ModelMismatchError(m, "hashing", HASHING_DIGEST)
        return HashingEmbedder(m.dims)
