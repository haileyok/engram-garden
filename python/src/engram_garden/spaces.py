"""An agent's access to the memory spaces it uses: one account, embedding provider and appview, and an
Agent for each space. Operations take a space by name or URI; an empty one is the default space,
except that recall searches every space. Mirrors internal/agent/spaces.go."""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from dataclasses import dataclass, field, replace
from datetime import datetime
from typing import Any

import httpx

from . import lex
from .agent import Agent, ForgetOut, GetOut, MemoriesOut, RememberOut
from .embed import HashingProvider, OpenAIProvider, Provider
from .errors import EngramError, SettingsError, SignInExpiredError, explain
from .identity import Directory
from .manage import ManageMixin
from .session import PdsSession
from .settings import ALL_SPACES, SIGN_IN_OAUTH, SIGN_IN_PASSWORD, Settings, SpaceEntry
from .space import Ref
from .spaceclient import Client

#: How long after a memory is stored to wait for the check that the appview can read its space.
INDEXING_CHECK_WAIT = 2.0


def space_of_uri(uri: str) -> str:
    """The space a record URI is in: at://{authority}/space/{type}/{skey}/{author}/{collection}/{rkey}."""
    parts = uri.removeprefix("at://").split("/") if uri.startswith("at://") else []
    if len(parts) != 7 or parts[1] != "space":
        raise EngramError(f"{uri!r} is not a record URI in a memory space")
    return "at://" + "/".join(parts[:4])


def _label(out: MemoriesOut, name: str) -> MemoriesOut:
    for m in out.memories:
        m.space = name
    return out


def indexing_problem(a: Agent, e: SpaceEntry, state: str) -> str:
    """What's wrong when the appview can't read a space (given its access state), or "" when nothing is."""
    if state in ("", "granted"):
        return ""
    why = "its authority hasn't let the appview index it"
    if state == "lapsed":
        why = "the appview's access to it has stopped working"
    todo = f"Its authority must approve indexing: in the web app, or with `engram index --space {e.name}`."
    try:
        ref = Ref.parse(e.uri)
    except ValueError:
        pass
    else:
        if ref.authority == a.client.did:
            todo = (
                f"This account governs it: call index_space (or run `engram index --space {e.name}`) and open "
                "the link it gives, signed in as this account."
            )
        else:
            todo = (
                f"Its authority, {ref.authority}, must approve indexing: in the web app, or with "
                f"`engram index --space {e.name}`."
            )
    return (
        f"the appview can't read this space ({state}: {why}), so what's stored in it can't be found by "
        f"searching. {todo}"
    )


async def _empty_note(a: Agent, e: SpaceEntry) -> str:
    """Explains an empty search: it may be that the appview can't read the space, not that it's empty."""
    try:
        st = await asyncio.wait_for(a.indexing(False), INDEXING_CHECK_WAIT)
    except Exception:  # noqa: BLE001
        return ""
    if p := indexing_problem(a, e, st):
        return "Nothing came back, and " + p
    return ""


@dataclass
class SpaceInfo:
    """Describes a space for agents: how to refer to it, and the embedding model it requires."""

    name: str
    uri: str
    default: bool = False
    #: False for a space the account belongs to that isn't in the settings.
    set_up: bool = False
    model: lex.ModelInfo | None = None
    next_model: lex.ModelInfo | None = None
    document_prefix: str = ""
    query_prefix: str = ""
    #: granted, missing or lapsed; empty when the appview doesn't say.
    indexing: str = ""
    warning: str = ""
    #: "ready", or what's wrong with this machine's embedding endpoint.
    local_model: str = ""
    error: str = ""


@dataclass
class ListSpacesOut:
    spaces: list[SpaceInfo] = field(default_factory=list)
    note: str = ""


class Spaces(ManageMixin):
    """An agent's access to the memory spaces it uses."""

    def __init__(
        self,
        client: Client,
        appview_url: str,
        appview_did: str,
        provider: Provider,
        settings: Settings | None = None,
        http: httpx.AsyncClient | None = None,
    ) -> None:
        self.client = client
        self.appview_url = appview_url
        self.appview_did = appview_did
        self.provider = provider
        #: Name the spaces and the default.
        self.settings = settings or Settings()
        self.http = http
        #: Builds a space's Agent, when set; tests use it to put each space on its own fake network.
        self.new_agent: Callable[[SpaceEntry], Agent] | None = None
        #: Writes the settings back after a space is created; None doesn't.
        self.save: Callable[[Settings], None] | None = None
        self._agents: dict[str, Agent] = {}
        self._owns_http = False

    # ---- settings ----

    def _settings(self) -> Settings:
        """A snapshot of the settings: create_space may add a space while other operations run."""
        return replace(self.settings, spaces=[replace(e) for e in self.settings.spaces])

    def save_settings(self) -> None:
        """Call save with a snapshot of the settings."""
        if self.save is not None:
            self.save(self._settings())

    def _add_space(self, uri: str) -> SpaceEntry:
        return self.settings.add_space(uri)

    def agent(self, name_or_uri: str = "") -> tuple[Agent, SpaceEntry]:
        """The Agent for a space, by name or URI ("" is the default)."""
        e = self._settings().resolve(name_or_uri)
        a = self._agents.get(e.uri)
        if a is None:
            if self.new_agent is not None:
                a = self.new_agent(e)
            else:
                a = Agent(self.client, e.uri, self.appview_url, self.appview_did, self.provider)
            self._agents[e.uri] = a
        return a, e

    async def aclose(self) -> None:
        """Close the HTTP client when this object opened it."""
        if self._owns_http and self.http is not None:
            await self.http.aclose()

    async def __aenter__(self) -> Spaces:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    # ---- remember, recall, get, list, forget ----

    async def remember(
        self,
        text: str,
        tags: list[str] | None = None,
        source: str = "",
        space: str = "",
        created_at: datetime | str | None = None,
    ) -> RememberOut:
        """Store a memory in a space (default: the default space)."""
        a, e = self.agent(space)
        # While the memory is embedded and stored, check the appview can read the space: if it can't,
        # the memory is stored and never searchable.
        check = asyncio.ensure_future(a.indexing(False))
        try:
            out = await a.remember(text, tags=tags, source=source, created_at=created_at)
        except BaseException:
            check.cancel()
            raise
        try:
            st = await asyncio.wait_for(asyncio.shield(check), INDEXING_CHECK_WAIT)
        except Exception:  # noqa: BLE001
            st = ""
        finally:
            if not check.done():
                check.cancel()
        if p := indexing_problem(a, e, st):
            out.note = "Stored, but " + p
        return out

    async def recall(
        self,
        query: str,
        limit: int = 0,
        author: str = "",
        tags: list[str] | None = None,
        since: datetime | str = "",
        space: str = "",
    ) -> MemoriesOut:
        """Search one space or, by default (or with space="all"), every space set up, merging the results by
        similarity. Each space's query is embedded with that space's model."""
        kw: dict[str, Any] = {"limit": limit, "author": author, "tags": tags, "since": since}
        if space and space != ALL_SPACES:
            a, e = self.agent(space)
            out = await a.recall(query, **kw)
            if not out.memories:
                out.note = (out.note + " " + await _empty_note(a, e)).strip()
            return _label(out, e.name)
        if not query.strip():
            raise EngramError("query is required")
        entries = self._settings().spaces
        if not entries:
            self._settings().check()
            raise SettingsError("no memory space set up")

        async def one(e: SpaceEntry) -> tuple[MemoriesOut | None, str, BaseException | None]:
            try:
                a, _ = self.agent(e.uri)
                out = await a.recall(query, **kw)
                note = await _empty_note(a, e) if not out.memories else ""
                return out, note, None
            except Exception as err:  # noqa: BLE001
                return None, "", err

        results = await asyncio.gather(*(one(e) for e in entries))
        merged = MemoriesOut()
        failed: list[str] = []
        notes: list[str] = []
        first_err: BaseException | None = None
        for e, (out, note, err) in zip(entries, results, strict=True):
            if err is not None or out is None:
                failed.append(f"{e.name} ({explain(err)})")
                first_err = first_err or err
                continue
            merged.memories.extend(_label(out, e.name).memories)
            if out.note:
                notes.append(f"{e.name}: {out.note}")
            if note:
                notes.append(f"{e.name}: {note}")
        if len(failed) == len(results):
            if len(results) == 1 and first_err is not None:
                raise first_err
            raise EngramError("recall failed in every space: " + "; ".join(failed))
        merged.memories.sort(key=lambda m: -(m.similarity or 0))  # stable
        n = limit if limit > 0 else 10
        merged.memories = merged.memories[: min(n, 50)]
        if failed:
            notes.append("Couldn't search " + "; ".join(failed) + ".")
        merged.note = " ".join(notes)
        return merged

    async def get(self, uri: str) -> GetOut:
        """Fetch a memory by URI, from whichever space the URI is in."""
        a, e = self.agent(space_of_uri(uri))
        out = await a.get(uri)
        out.memory.space = e.name
        return out

    async def list(
        self, limit: int = 0, cursor: str = "", author: str = "", tags: list[str] | None = None, space: str = ""
    ) -> MemoriesOut:
        """List one space's memories, newest first (default: the default space)."""
        if space == ALL_SPACES:
            raise EngramError("list one space at a time; recall searches every space")
        a, e = self.agent(space)
        return _label(await a.list(limit=limit, cursor=cursor, author=author, tags=tags), e.name)

    async def forget(self, uri: str) -> ForgetOut:
        """Delete one of the agent's own memories, by URI."""
        a, _ = self.agent(space_of_uri(uri))
        return await a.forget(uri)

    # ---- list_spaces ----

    async def list_spaces(self) -> ListSpacesOut:
        """Describe the spaces set up, with each one's model and whether this machine can embed with it,
        then any other spaces the account's PDS lists for it."""
        st = self._settings()
        default = st.default()
        infos = list(await asyncio.gather(*(self._describe(e) for e in st.spaces)))
        for info in infos:
            info.default = default is not None and info.uri == default.uri
        out = ListSpacesOut(spaces=infos)
        try:
            others = await self._member_spaces()
        except Exception as err:  # noqa: BLE001
            out.note = "Couldn't list other spaces this account belongs to: " + str(explain(err))
            others = []
        for uri in others:
            if any(i.uri == uri for i in infos):
                continue
            e = st.resolve(uri)
            out.spaces.append(SpaceInfo(name=e.name, uri=uri))
        return out

    async def _describe(self, e: SpaceEntry) -> SpaceInfo:
        info = SpaceInfo(name=e.name, uri=e.uri, set_up=True)
        try:
            a, _ = self.agent(e.uri)
        except Exception as err:  # noqa: BLE001
            info.error = str(err)
            return info
        try:
            state = await a.indexing(True)
            info.indexing = state
            if p := indexing_problem(a, e, state):
                info.warning = p[:1].upper() + p[1:]
        except Exception:  # noqa: BLE001
            pass
        try:
            cfg = await a.config(False)
        except Exception as err:  # noqa: BLE001
            info.error = str(explain(err))
            return info
        info.model, info.next_model = cfg.model_info, cfg.next
        info.document_prefix, info.query_prefix = cfg.document_prefix, cfg.query_prefix
        try:
            await self.provider.for_model(cfg.model_info)
            info.local_model = "ready"
        except Exception as err:  # noqa: BLE001
            info.local_model = str(err)
        return info

    async def _member_spaces(self) -> list[str]:
        """The memory spaces the account's PDS has for it."""
        uris: list[str] = []
        cursor = ""
        for _ in range(10):
            params: dict[str, Any] = {"spaceType": lex.SPACE_TYPE, "limit": 100}
            if cursor:
                params["cursor"] = cursor
            out = await self.client.session.get("com.atproto.space.listSpaces", params)
            spaces = out.get("spaces", [])
            uris.extend(sp["uri"] for sp in spaces)
            cursor = out.get("cursor", "")
            if not cursor or not spaces:
                break
        return uris

    async def run(self, every: float) -> None:
        """Warm every space set up and keep this agent's memories in each embedded with the space's
        model(s), until cancelled."""
        agents = []
        for e in self._settings().spaces:
            try:
                agents.append(self.agent(e.uri)[0])
            except Exception:  # noqa: BLE001
                continue
        await asyncio.gather(*(a.run(every) for a in agents))


def spaces_of(*agents: Agent) -> Spaces:
    """Put agents for different spaces together, each keeping its own client. The first agent's space is
    the default; the first agent's client lists the account's spaces."""
    first = agents[0]
    s = Spaces(first.client, first.appview_url, first.appview_did, first.provider)
    by_uri = {}
    for a in agents:
        s.settings.add_space(a.space)
        by_uri[a.space] = a

    def new_agent(e: SpaceEntry) -> Agent:
        return by_uri.get(e.uri) or Agent(s.client, e.uri, s.appview_url, s.appview_did, s.provider)

    s.new_agent = new_agent
    return s


# ---- opening ----


def make_provider(settings: Settings, http: httpx.AsyncClient | None = None) -> Provider:
    """Build the embedding provider from settings.embed."""
    p = settings.embed.provider
    if p == "hashing":
        return HashingProvider()
    if p in ("", "openai"):
        return OpenAIProvider(
            base_url=settings.embed.url or "http://localhost:11434/v1",
            api_key=settings.embed.api_key,
            model_name=settings.embed.model,
            digest=settings.embed.digest,
            http=http,
        )
    raise SettingsError(f'unknown embedding provider "{p}" (openai or hashing)')


async def open_session(settings: Settings, http: httpx.AsyncClient, directory: Directory) -> PdsSession:
    """Open the account's session at its PDS."""
    a = settings.account
    if a.sign_in == SIGN_IN_PASSWORD:
        pds = a.pds_host
        if not pds:
            ident = await directory.lookup(a.handle)
            pds = ident.pds_endpoint()
            if not pds:
                raise EngramError(f"{a.handle} publishes no PDS")
        return await PdsSession.login(http, pds, a.handle, a.password)
    if a.sign_in == SIGN_IN_OAUTH:
        raise SignInExpiredError(
            "this account is signed in with OAuth, which the Python library can't resume: use password sign-in "
            "(an app password works)"
        )
    settings.check_account()
    raise SettingsError("not signed in")


async def open_spaces(
    settings: Settings,
    *,
    http: httpx.AsyncClient | None = None,
    directory: Directory | None = None,
) -> Spaces:
    """Sign in and return the agent's spaces. It needs no space set up: list_spaces then lists the
    ones the account belongs to."""
    settings.check_account()
    own = http is None
    client_http = http or httpx.AsyncClient(timeout=60, follow_redirects=False)
    try:
        directory = directory or Directory(client_http)
        provider = make_provider(settings, client_http)
        sess = await open_session(settings, client_http, directory)
        client = Client(sess, directory, client_http)
    except BaseException:
        if own:
            await client_http.aclose()
        raise
    s = Spaces(client, settings.appview_url, settings.appview_did, provider, settings, client_http)
    s._owns_http = own
    return s
