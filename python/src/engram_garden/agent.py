"""One agent's access to a memory space: remember embeds a memory and writes it to the agent's own
repo, and recall embeds the query and searches the whole space through the appview. Embedding
happens here, on the agent's side, with the model the space declares. Mirrors internal/agent/agent.go."""

from __future__ import annotations

import asyncio
import copy
import logging
import time
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from . import lex, vec
from .embed import Provider
from .errors import EngramError, ModelMismatchError, XrpcError, is_error
from .space import Ref
from .spaceclient import Client

log = logging.getLogger("engram_garden")

#: How long a space the appview may read is assumed to stay readable, so remembering doesn't ask every time.
GRANTED_TTL = 2 * 60.0
CONFIG_TTL = 5 * 60.0


@dataclass
class Memory:
    """A memory as tools return it."""

    uri: str
    author: str
    text: str
    tags: list[str] = field(default_factory=list)
    source: str = ""
    created_at: str = ""
    #: Search results only: cosine similarity to the query, scaled to 0-1000.
    similarity: int | None = None
    #: The name of the memory space the memory is in.
    space: str = ""
    cid: str = ""
    indexed_at: str = ""

    @classmethod
    def from_view(cls, d: dict[str, Any]) -> Memory:
        """From the appview's memoryView."""
        return cls(
            uri=d.get("uri", ""),
            author=d.get("author", ""),
            text=d.get("text", ""),
            tags=list(d.get("tags") or []),
            source=d.get("source", "") or "",
            created_at=d.get("createdAt", "") or "",
            similarity=d.get("similarity"),
            cid=d.get("cid", "") or "",
            indexed_at=d.get("indexedAt", "") or "",
        )

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"uri": self.uri, "author": self.author, "text": self.text, "tags": self.tags}
        if self.space:
            d["space"] = self.space
        if self.source:
            d["source"] = self.source
        d["createdAt"] = self.created_at
        if self.similarity is not None:
            d["similarity"] = self.similarity
        return d


@dataclass
class MemoriesOut:
    memories: list[Memory] = field(default_factory=list)
    cursor: str = ""
    #: Explains results that may be less precise than usual.
    note: str = ""


@dataclass
class RememberOut:
    uri: str = ""
    cid: str = ""
    #: What to know about the memory, such as that it can't be found by searching yet.
    note: str = ""


@dataclass
class GetOut:
    memory: Memory


@dataclass
class ForgetOut:
    deleted: str


def unwrap(err: BaseException) -> BaseException:
    """Turn an XRPC error into a short message for the agent."""
    if isinstance(err, XrpcError) and err.message:
        return EngramError(f"{err.name}: {err.message}")
    return err


def _truncate(s: str, n: int) -> str:
    b = s.encode()
    return s if len(b) <= n else b[:n].decode(errors="ignore")


def _time_str(t: datetime | str | None) -> str:
    if t is None:
        return lex.format_time(datetime.now().astimezone())
    if isinstance(t, datetime):
        return lex.format_time(t)
    return t


class Agent:
    """One agent's access to a memory space."""

    def __init__(
        self,
        client: Client,
        space: str,
        appview_url: str,
        appview_did: str,
        provider: Provider,
        config_ttl: float = CONFIG_TTL,
    ) -> None:
        #: Acts as the agent's account.
        self.client = client
        #: The memory space URI.
        self.space = space
        #: Locate the appview that indexes the space.
        self.appview_url = appview_url
        self.appview_did = appview_did
        #: Embeds with the space's model, after checking the local model matches it.
        self.provider = provider
        self.config_ttl = config_ttl
        self._cfg: lex.Config | None = None
        self._cfg_at = 0.0
        self._indexing = ""
        self._indexing_at = 0.0
        self.reembeds = 0  # memories rewritten with missing vectors

    # ---- the space's model ----

    def _authority(self) -> str:
        return Ref.parse(self.space).authority

    def forget_config(self) -> None:
        """Reread the space's config next time it's needed."""
        self._cfg = None

    async def config(self, refresh: bool = False) -> lex.Config:
        """The space's declared model, from the authority's garden.engram.config record, cached."""
        ttl = self.config_ttl if self.config_ttl > 0 else CONFIG_TTL
        if not refresh and self._cfg is not None and time.monotonic() - self._cfg_at < ttl:
            return self._cfg
        auth = self._authority()
        host = await self.client.pds_host(auth)
        params = {"space": self.space, "repo": auth, "collection": lex.CONFIG_COLLECTION, "rkey": lex.CONFIG_RKEY}
        try:
            out = await self.client.query(host, self.space, auth, "com.atproto.space.getRecord", params)
        except XrpcError as e:
            if is_error(e, "RecordNotFound", "RepoNotFound"):
                raise EngramError(
                    "the space hasn't declared an embedding model yet: its authority needs to declare one "
                    "(engram model --set <model>)"
                ) from e
            raise EngramError(f"reading the space's config: {e}") from e
        c = lex.parse_config(out["value"])
        self._cfg, self._cfg_at = c, time.monotonic()
        return c

    async def _embed_one(self, m: lex.ModelInfo, text: str) -> list[float]:
        """Embed a text with a model, normalized."""
        e = await self.provider.for_model(m)
        vs = await e.embed([text])
        v = vs[0]
        if len(v) != m.dims:
            raise EngramError(f"the model returned {len(v)} dimensions; the space expects {m.dims}")
        if not vec.normalize(v):
            raise EngramError("the model returned a zero vector")
        return v

    async def _add_embeddings(self, cfg: lex.Config, rec: dict[str, Any], text: str, tags: list[str]) -> None:
        """Set a memory record's vectors for the space's model and, during a model change, the next one.
        The next model is best effort: an agent without it still writes a memory the active index can use."""
        doc = lex.embed_text(cfg.document_prefix, text, tags)
        v = await self._embed_one(cfg.model_info, doc)
        rec["embedding"] = lex.embedding_record(cfg.model_info, v)
        rec.pop("nextEmbedding", None)
        if cfg.next is not None:
            try:
                nv = await self._embed_one(cfg.next, doc)
            except (EngramError, ModelMismatchError) as e:
                log.warning(
                    "the space is moving to a new model this agent can't use; writing the current model's "
                    "vector only (next=%s, err=%s)",
                    cfg.next,
                    e,
                )
                return
            rec["nextEmbedding"] = lex.embedding_record(cfg.next, nv)

    # ---- remember ----

    async def remember(
        self,
        text: str,
        tags: list[str] | None = None,
        source: str = "",
        created_at: datetime | str | None = None,
    ) -> RememberOut:
        """Embed a memory and store it in the agent's own repo.

        created_at is an extension of the Go library: it lets a migration keep a memory's original time."""
        text = text.strip()
        if not text:
            raise EngramError("text is required")
        if len(text.encode()) > 30000:
            raise EngramError("text is too long (max 30000 bytes); split it into several memories")
        if tags and len(tags) > 16:
            raise EngramError("at most 16 tags")
        rec: dict[str, Any] = {
            "$type": lex.MEMORY_COLLECTION,
            "text": text,
            "createdAt": _time_str(created_at),
        }
        clean: list[str] = []
        for tag in tags or []:
            tag = tag.strip()
            if tag:
                if len(tag.encode()) > 128:
                    raise EngramError(f"tag {tag!r} is too long")
                clean.append(tag)
        if clean:
            rec["tags"] = clean
        if s := source.strip():
            rec["source"] = s
        cfg = await self.config(False)
        try:
            await self._add_embeddings(cfg, rec, text, clean)
        except ModelMismatchError as e:
            # Keep the type (callers catch it), as Go's wrapped error keeps its.
            e.args = (f"not stored: {e}",)
            raise
        except EngramError as e:
            raise EngramError(f"not stored: {e}") from e
        body = {
            "space": self.space,
            "repo": self.client.did,
            "collection": lex.MEMORY_COLLECTION,
            "record": rec,
        }
        try:
            out = await self.client.session.post("com.atproto.space.createRecord", body)
        except XrpcError as e:
            raise EngramError(f"storing memory: {e}") from e
        out = out or {}
        return RememberOut(uri=out.get("uri", ""), cid=out.get("cid", ""))

    # ---- recall ----

    async def recall(
        self,
        query: str,
        limit: int = 0,
        author: str = "",
        tags: list[str] | None = None,
        since: datetime | str = "",
    ) -> MemoriesOut:
        """Search every agent's memories by meaning."""
        if not query.strip():
            raise EngramError("query is required")
        out: dict[str, Any] = {}
        for attempt in range(2):
            cfg = await self.config(attempt > 0)
            v = await self._embed_one(cfg.model_info, cfg.query_prefix + query)
            params: dict[str, Any] = {
                "space": self.space,
                "q": _truncate(query, 4000),
                "vector": lex.encode_query_vector(v),
                "model": cfg.model,
                "modelDigest": cfg.model_digest,
            }
            if limit:
                params["limit"] = min(max(limit, 1), 50)
            if author:
                params["author"] = author
            if since:
                params["since"] = _time_str(since)
            if tags:
                params["tags"] = tags
            try:
                out = await self._query("garden.engram.searchMemories", params)
            except XrpcError as e:
                if attempt == 0 and e.name == "ModelMismatch":
                    continue  # the space changed models: re-read the config
                raise unwrap(e) from e
            break
        res = _memories(out)
        if out.get("approximate"):
            res.note = (
                "The index was still loading, so these results are ranked coarsely and may be less precise. "
                "Recalling again shortly gives exact ranking."
            )
        return res

    # ---- get, list, forget ----

    async def get(self, uri: str) -> GetOut:
        """Fetch one memory by URI."""
        try:
            out = await self._query("garden.engram.getMemory", {"space": self.space, "uri": uri})
        except XrpcError as e:
            raise unwrap(e) from e
        return GetOut(memory=Memory.from_view(out.get("memory", {})))

    async def list(
        self, limit: int = 0, cursor: str = "", author: str = "", tags: list[str] | None = None
    ) -> MemoriesOut:
        """List memories newest first."""
        params: dict[str, Any] = {"space": self.space}
        if limit:
            params["limit"] = min(max(limit, 1), 100)
        if cursor:
            params["cursor"] = cursor
        if author:
            params["author"] = author
        if tags:
            params["tags"] = tags
        try:
            out = await self._query("garden.engram.listMemories", params)
        except XrpcError as e:
            raise unwrap(e) from e
        return _memories(out)

    async def forget(self, uri: str) -> ForgetOut:
        """Delete one of the agent's own memories."""
        author, coll, rkey = self.parse_uri(uri)
        if author != self.client.did:
            raise EngramError(f"that memory belongs to {author}; you can only forget your own")
        if coll != lex.MEMORY_COLLECTION:
            raise EngramError(f"not a memory: {uri}")
        body = {"space": self.space, "repo": author, "collection": coll, "rkey": rkey}
        try:
            await self.client.session.post("com.atproto.space.deleteRecord", body)
        except XrpcError as e:
            raise EngramError(f"deleting memory: {e}") from e
        return ForgetOut(deleted=uri)

    def parse_uri(self, uri: str) -> tuple[str, str, str]:
        """Split a space record URI into author, collection and rkey."""
        prefix = str(Ref.parse(self.space)) + "/"
        parts = uri.removeprefix(prefix).split("/") if uri.startswith(prefix) else []
        if len(parts) != 3 or not all(parts):
            raise EngramError(f"{uri!r} is not a record URI in {self.space}")
        return parts[0], parts[1], parts[2]

    async def _query(self, nsid: str, params: dict[str, Any]) -> dict[str, Any]:
        out = await self.client.query(self.appview_url, self.space, self.appview_did, nsid, params)
        return out or {}

    # ---- whether the appview can index the space ----

    async def indexing(self, fresh: bool = False) -> str:
        """Whether the appview may read the space, and so index what's stored in it: "granted";
        "missing" (its authority never let the appview); "lapsed" (it did, and the grant stopped
        working); or "" when the appview doesn't say. Unless fresh, a recent "granted" is trusted."""
        if not fresh and self._indexing == "granted" and time.monotonic() - self._indexing_at < GRANTED_TTL:
            return "granted"
        try:
            st = await self._query("garden.engram.getSpaceStatus", {"space": self.space})
        except XrpcError as e:
            raise unwrap(e) from e
        state = (st.get("access") or {}).get("state", "")
        self._indexing, self._indexing_at = state, time.monotonic()
        return state

    # ---- session start and model changes ----

    async def warm(self) -> None:
        """Ask the appview to start loading the space's index, so the first recall is fast."""
        await self.client.procedure(
            self.appview_url, self.space, self.appview_did, "garden.engram.warmSpace", {"space": self.space}
        )

    async def reembed(self) -> int:
        """Rewrite this agent's memories that lack a vector for the space's model or, during a model
        change, for the next model. Returns how many it rewrote."""
        cfg = await self.config(True)
        want = [cfg.model_info] + ([cfg.next] if cfg.next is not None else [])
        me = self.client.did
        rewritten = 0
        cursor = ""
        while True:
            params: dict[str, Any] = {
                "space": self.space,
                "repo": me,
                "collection": lex.MEMORY_COLLECTION,
                "limit": 100,
            }
            if cursor:
                params["cursor"] = cursor
            try:
                page = await self.client.session.get("com.atproto.space.listRecords", params)
            except XrpcError as e:
                raise EngramError(f"listing my memories: {e}") from e
            records = page.get("records", [])
            for r in records:
                value = r.get("value")
                if not isinstance(value, dict):
                    continue
                have = {e.key() for e in lex.parse_embeddings(value)}
                if all(m.key() in have for m in want):
                    continue
                text = value.get("text", "")
                if not isinstance(text, str) or not text.strip():
                    continue
                tags = [t for t in value.get("tags", []) or [] if isinstance(t, str)]
                # Rewrite from the JSON form, which keeps $bytes and $link values intact.
                out = copy.deepcopy(value)
                await self._add_embeddings(cfg, out, text, tags)
                try:
                    _, _, rkey = self.parse_uri(r["uri"])
                except EngramError:
                    continue
                body = {
                    "space": self.space,
                    "repo": me,
                    "collection": lex.MEMORY_COLLECTION,
                    "rkey": rkey,
                    "record": out,
                }
                try:
                    await self.client.session.post("com.atproto.space.putRecord", body)
                except XrpcError as e:
                    raise EngramError(f"rewriting {r['uri']}: {e}") from e
                rewritten += 1
            cursor = page.get("cursor", "")
            if not cursor or not records:
                break
        self.reembeds += rewritten
        return rewritten

    async def run(self, every: float) -> None:
        """Warm the space, then keep this agent's memories embedded with the space's model(s) until cancelled."""
        try:
            await self.warm()
        except Exception as e:  # noqa: BLE001 - warming is best effort
            log.debug("warming the space failed: %s", e)
        while True:
            try:
                n = await self.reembed()
                if n:
                    log.info("re-embedded %d memories for the space's model", n)
            except asyncio.CancelledError:
                raise
            except Exception as e:  # noqa: BLE001
                log.warning("re-embedding memories failed: %s", e)
            await asyncio.sleep(every)


def _memories(out: dict[str, Any]) -> MemoriesOut:
    return MemoriesOut(
        memories=[Memory.from_view(m) for m in out.get("memories", []) or []],
        cursor=out.get("cursor", "") or "",
    )
