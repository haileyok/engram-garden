"""Running spaces: creating them, their members, their model and whether the appview indexes them.
These act as the space's authority, so they need the account to be the one that created the space.
Mirrors internal/agent/manage.go."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any
from urllib.parse import urlencode

import httpx

from . import embed, lex
from .errors import EngramError, XrpcError, explain
from .settings import Settings, SpaceEntry
from .space import Ref, valid_did, valid_record_key

if TYPE_CHECKING:
    from .agent import Agent
    from .spaceclient import Client


def manage_err(what: str, err: BaseException) -> EngramError:
    """Adds what to do when the account's server refuses for lack of permission."""
    if isinstance(err, XrpcError) and (err.status in (401, 403) or "scope" in (err.name + err.message).lower()):
        return EngramError(
            f"{what}: {err} (if this sign-in is from before engram could run spaces, sign in again to grant it)"
        )
    return EngramError(f"{what}: {explain(err)}")


@dataclass
class CreateSpaceOut:
    space: SpaceEntry
    #: The declared model, if one was.
    model: lex.ModelInfo | None = None
    #: What's left before agents can use the space.
    next: str = ""


@dataclass
class Member:
    did: str
    handle: str = ""
    read: bool = False
    write: bool = False


@dataclass
class MembersOut:
    space: str
    members: list[Member] = field(default_factory=list)


@dataclass
class MemberOut:
    space: str
    member: Member


@dataclass
class ModelOut:
    space: str
    model: lex.ModelInfo | None = None
    next_model: lex.ModelInfo | None = None
    document_prefix: str = ""
    query_prefix: str = ""


@dataclass
class IndexOut:
    space: str
    #: The appview's access to the space: granted, missing, lapsed, or empty when it can't say.
    state: str = ""
    #: The appview's page where the authority approves (or stops) indexing, signed in as the authority.
    link: str = ""
    note: str = ""


def _model_out(name: str, cfg: lex.Config | None) -> ModelOut:
    out = ModelOut(space=name)
    if cfg is not None:
        out.model, out.next_model = cfg.model_info, cfg.next
        out.document_prefix, out.query_prefix = cfg.document_prefix, cfg.query_prefix
    return out


async def describe_model(
    settings: Settings, name: str, dims: int = 0, http: httpx.AsyncClient | None = None
) -> lex.ModelInfo:
    """Identify a local model exactly: its name, its digest (from Ollama, or the configured digest) and its
    dimensions (from embedding a probe). The offline hashing provider takes dims as given."""
    if not name.strip():
        raise EngramError("which model? e.g. nomic-embed-text")
    if settings.embed.provider == "hashing":
        if dims <= 0:
            raise EngramError("dims is required with the hashing provider")
        return lex.ModelInfo(model=name, model_digest=embed.HASHING_DIGEST, dims=dims)
    base = settings.embed.url or embed.DEFAULT_BASE_URL
    own = http is None
    client = http or httpx.AsyncClient(timeout=60)
    try:
        digest = settings.embed.digest
        if not digest:
            digest = await embed.ollama_digest(client, base, name)
            if not digest:
                raise EngramError(f"{name} isn't installed in Ollama: run ollama pull {name}")
        try:
            n = await embed.probe_dims(base, settings.embed.api_key, name, client)
        except EngramError as e:
            raise EngramError(f"embedding a probe with {name}: {e}") from e
    finally:
        if own:
            await client.aclose()
    return lex.ModelInfo(model=name, model_digest=digest, dims=n)


class ManageMixin:
    """The space-running half of Spaces."""

    client: Client
    appview_url: str
    http: httpx.AsyncClient | None

    # Provided by Spaces.
    def agent(self, name_or_uri: str = "") -> tuple[Agent, SpaceEntry]: ...  # type: ignore[empty-body]
    def _settings(self) -> Settings: ...  # type: ignore[empty-body]
    def _add_space(self, uri: str) -> SpaceEntry: ...  # type: ignore[empty-body]

    def _owned(self, name_or_uri: str) -> tuple[SpaceEntry, Ref]:
        """Resolve a space the account governs."""
        e = self._settings().resolve(name_or_uri)
        ref = Ref.parse(e.uri)
        me = self.client.did
        if ref.authority != me:
            raise EngramError(f"only the space's authority ({ref.authority}) can do that; this account is {me}")
        return e, ref

    # ---- create_space ----

    async def create_space(self, name: str, model: str = "", dims: int = 0) -> CreateSpaceOut:
        """Create a memory space governed by this account, add it to the settings (the default, if it's
        the first), and optionally declare its model. Call save_settings afterwards to keep it."""
        if not valid_record_key(name):
            raise EngramError("names use letters, digits and . _ ~ : - (up to 512)")
        member = {"$type": "com.atproto.simplespace.defs#memberListPolicy"}
        body = {"spaceType": lex.SPACE_TYPE, "skey": name, "readPolicy": member, "writePolicy": member}
        try:
            out = await self.client.session.post("com.atproto.simplespace.createSpace", body)
        except XrpcError as e:
            raise manage_err("creating the space", e) from e
        e = self._add_space(out["uri"])
        res = CreateSpaceOut(space=e)
        if model:
            try:
                cfg = await self.set_model(space=e.uri, action=lex.DECLARE, model=model, dims=dims)
                res.model = cfg.model
            except EngramError as err:
                res.next = (
                    f"The space exists, but declaring its model failed ({err}): run set_model / engram model --set."
                )
                return res
        if res.model is None:
            res.next = (
                "Declare its embedding model (set_model / engram model --set <model>), add members, and let the "
                "appview index it (index_space / engram index)."
            )
        else:
            res.next = (
                "Add members (add_member / engram members add) and let the appview index it "
                "(index_space / engram index)."
            )
        return res

    # ---- members ----

    async def list_members(self, space: str = "") -> MembersOut:
        """List a space's members, with their handles when they resolve."""
        e, ref = self._owned(space)
        out = MembersOut(space=e.name)
        cursor = ""
        for _ in range(50):
            params: dict[str, Any] = {"space": str(ref), "limit": 100}
            if cursor:
                params["cursor"] = cursor
            try:
                page = await self.client.session.get("com.atproto.simplespace.listMembers", params)
            except XrpcError as err:
                raise manage_err("listing members", err) from err
            members = page.get("members", [])
            out.members.extend(
                Member(did=m["did"], handle=m.get("handle", ""), read=bool(m.get("read")), write=bool(m.get("write")))
                for m in members
            )
            cursor = page.get("cursor", "")
            if not cursor or not members:
                break
        for m in out.members:
            if valid_did(m.did):
                try:
                    ident = await self.client.dir.lookup_did(m.did)
                    if ident.handle:
                        m.handle = ident.handle
                except Exception:  # noqa: BLE001
                    pass
        return out

    async def _resolve_member(self, raw: str) -> str:
        raw = raw.strip().removeprefix("@")
        if raw.startswith("did:"):
            if not valid_did(raw):
                raise EngramError(f"{raw!r} isn't a handle or DID")
            return raw
        try:
            return await self.client.dir.resolve_handle(raw)
        except EngramError as e:
            raise EngramError(f"couldn't resolve {raw}: {e}") from e

    async def add_member(self, member: str, space: str = "", read_only: bool = False) -> MemberOut:
        """Add an account to a space, or change what it may do there."""
        e, ref = self._owned(space)
        did = await self._resolve_member(member)
        m = Member(did=did, read=True, write=not read_only)
        if not member.startswith("did:"):
            m.handle = member.strip().removeprefix("@")
        body = {"space": str(ref), "did": did, "read": m.read, "write": m.write}
        try:
            await self.client.session.post("com.atproto.simplespace.putMember", body)
        except XrpcError as err:
            raise manage_err("adding the member", err) from err
        return MemberOut(space=e.name, member=m)

    async def remove_member(self, member: str, space: str = "") -> MemberOut:
        """Remove an account from a space. Its memories stay in its own repo, but the appview stops indexing them."""
        e, ref = self._owned(space)
        did = await self._resolve_member(member)
        try:
            await self.client.session.post("com.atproto.simplespace.removeMember", {"space": str(ref), "did": did})
        except XrpcError as err:
            raise manage_err("removing the member", err) from err
        return MemberOut(space=e.name, member=Member(did=did))

    # ---- model ----

    async def _current_config(self, ref: Ref) -> lex.Config | None:
        """The space's config record from the authority's repo, or None when there's none."""
        params = {
            "space": str(ref),
            "repo": ref.authority,
            "collection": lex.CONFIG_COLLECTION,
            "rkey": lex.CONFIG_RKEY,
        }
        try:
            out = await self.client.session.get("com.atproto.space.getRecord", params)
        except XrpcError as e:
            if e.name in ("RecordNotFound", "RepoNotFound"):
                return None
            raise manage_err("reading the space's model", e) from e
        return lex.parse_config(out["value"])

    async def model(self, space: str = "") -> ModelOut:
        """Show a space's model, from its authority's record."""
        a, e = self.agent(space)
        cfg = await a.config(True)
        return _model_out(e.name, cfg)

    async def set_model(
        self,
        action: str,
        space: str = "",
        model: str = "",
        dims: int = 0,
        document_prefix: str = "",
        query_prefix: str = "",
    ) -> ModelOut:
        """Declare or change a space's embedding model: declare, start moving to the next model
        ("next"), promote it, or cancel the move."""
        e, ref = self._owned(space)
        cur = await self._current_config(ref)
        m = lex.ModelInfo()
        if action in (lex.DECLARE, lex.START_NEXT):
            m = await describe_model(self._settings(), model, dims, self.http)
        elif action not in (lex.PROMOTE, lex.CANCEL_NEXT):
            raise EngramError(f"action must be {lex.DECLARE}, {lex.START_NEXT}, {lex.PROMOTE} or {lex.CANCEL_NEXT}")
        try:
            cfg = lex.change_config(cur, action, m, document_prefix, query_prefix)
        except ValueError as err:
            raise EngramError(str(err)) from err
        body = {
            "space": str(ref),
            "repo": ref.authority,
            "collection": lex.CONFIG_COLLECTION,
            "rkey": lex.CONFIG_RKEY,
            "record": cfg.record(),
        }
        try:
            await self.client.session.post("com.atproto.space.putRecord", body)
        except XrpcError as err:
            raise manage_err("writing the space's model", err) from err
        # Reread it next time it's needed.
        try:
            self.agent(e.uri)[0].forget_config()
        except Exception:  # noqa: BLE001
            pass
        return _model_out(e.name, cfg)

    # ---- index_space ----

    async def index_state(self, name_or_uri: str = "") -> str:
        """Whether the appview may read the space: granted, missing or lapsed, or "" when it doesn't say."""
        a, _ = self.agent(name_or_uri)
        return await a.indexing(True)

    async def index_space(self, space: str = "", stop: bool = False) -> IndexOut:
        """The appview's page for letting it index the space (or stopping it), and its current state.
        Approving takes the authority's browser, so an agent passes the link to its person."""
        e, ref = self._owned(space)
        out = IndexOut(space=e.name)
        try:
            out.state = await self.index_state(e.uri)
        except EngramError as err:
            out.note = "couldn't read the appview's access: " + str(err)
        if (out.state == "granted") != stop:
            return out  # already as asked
        grant = await self._grant_url()
        q = urlencode({"space": str(ref), "mode": "stop" if stop else "grant"})
        out.link = grant + "?" + q
        out.note = (
            f"Open the link in a browser and sign in as {ref.authority} to approve; the state changes once you do."
        )
        return out

    async def _grant_url(self) -> str:
        """Ask the appview where its grant page is."""
        client = self.http or self.client.http
        resp = await client.get(self.appview_url.rstrip("/") + "/xrpc/garden.engram.describeService")
        if resp.status_code != 200:
            raise EngramError(f"asking the appview at {self.appview_url}: {resp.status_code}")
        url = resp.json().get("grantUrl", "")
        if not url:
            raise EngramError(f"the appview at {self.appview_url} doesn't take indexing grants")
        return url
