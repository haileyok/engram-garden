"""Settings: the file `engram login` and the space commands write (the same file, same format),
with ENGRAM_* environment variables taking priority. Mirrors internal/agent/settings.go."""

from __future__ import annotations

import json
import os
import tempfile
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from .errors import SettingsError
from .space import Ref

DEFAULT_APPVIEW_URL = "https://api.engram.garden"

#: How an agent signs in to its account.
SIGN_IN_OAUTH = "oauth"
SIGN_IN_PASSWORD = "password"

#: How long an OAuth sign-in lasts.
OAUTH_SESSION_LIFETIME = timedelta(days=14)

#: The space argument that recalls from every configured space. It can't name a space.
ALL_SPACES = "all"


def _check_space_name(name: str) -> None:
    if not name.strip() or name != name.strip():
        raise SettingsError("a space name can't be empty or start or end with spaces")
    if name == ALL_SPACES:
        raise SettingsError(f'"{ALL_SPACES}" is reserved: it means every space')
    if "/" in name or ":" in name:
        raise SettingsError(f"space name {name!r} can't contain / or : (those are for URIs)")


def _space_key(uri: str) -> str:
    try:
        return Ref.parse(uri).skey
    except ValueError:
        return ""


@dataclass
class SpaceEntry:
    """A memory space the agent uses. name is how it's referred to; it defaults to the space's key."""

    name: str = ""
    uri: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {"name": self.name, "uri": self.uri}


@dataclass
class Account:
    """The agent's own ATProto account and how it signs in."""

    handle: str = ""
    did: str = ""
    #: SIGN_IN_OAUTH or SIGN_IN_PASSWORD.
    sign_in: str = ""
    password: str = ""
    #: Skips resolving the account's PDS (password sign-in).
    pds_host: str = ""
    session_id: str = ""
    callback: str = ""
    signed_in_at: datetime | None = None

    def expires(self) -> datetime | None:
        """When an OAuth sign-in ends, or None for a password."""
        if self.sign_in != SIGN_IN_OAUTH or self.signed_in_at is None:
            return None
        return self.signed_in_at + OAUTH_SESSION_LIFETIME

    def to_dict(self) -> dict[str, Any]:
        d = {
            "handle": self.handle,
            "did": self.did,
            "signIn": self.sign_in,
            "password": self.password,
            "pdsHost": self.pds_host,
            "sessionId": self.session_id,
            "callback": self.callback,
        }
        d = {k: v for k, v in d.items() if v}
        if self.signed_in_at is not None:
            d["signedInAt"] = self.signed_in_at.isoformat().replace("+00:00", "Z")
        return d

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> Account:
        signed = d.get("signedInAt")
        return cls(
            handle=d.get("handle", ""),
            did=d.get("did", ""),
            sign_in=d.get("signIn", ""),
            password=d.get("password", ""),
            pds_host=d.get("pdsHost", ""),
            session_id=d.get("sessionId", ""),
            callback=d.get("callback", ""),
            signed_in_at=datetime.fromisoformat(signed) if signed and not signed.startswith("0001") else None,
        )


@dataclass
class Embed:
    """Configures the embedding endpoint."""

    #: An OpenAI-compatible base URL (default Ollama's).
    url: str = ""
    api_key: str = ""
    #: The local model's name, when it differs from the space's.
    model: str = ""
    #: The local model's digest, for endpoints that aren't Ollama.
    digest: str = ""
    #: "openai" (default) or "hashing" (offline, for tests).
    provider: str = ""

    def to_dict(self) -> dict[str, Any]:
        d = {
            "url": self.url,
            "apiKey": self.api_key,
            "model": self.model,
            "digest": self.digest,
            "provider": self.provider,
        }
        return {k: v for k, v in d.items() if v}

    @classmethod
    def from_dict(cls, d: Mapping[str, Any]) -> Embed:
        return cls(
            url=d.get("url", ""),
            api_key=d.get("apiKey", ""),
            model=d.get("model", ""),
            digest=d.get("digest", ""),
            provider=d.get("provider", ""),
        )


@dataclass
class Settings:
    #: The memory spaces the agent uses, each with a short name.
    spaces: list[SpaceEntry] = field(default_factory=list)
    #: Names the space used when none is given.
    default_space: str = ""
    appview_url: str = ""
    appview_did: str = ""
    account: Account = field(default_factory=Account)
    embed: Embed = field(default_factory=Embed)
    #: Keys this library doesn't know, kept so saving doesn't drop another tool's fields.
    extra: dict[str, Any] = field(default_factory=dict)

    # ---- spaces ----

    def _entry(self, name_or_uri: str) -> int:
        for i, e in enumerate(self.spaces):
            if name_or_uri in (e.name, e.uri):
                return i
        return -1

    def add_space(self, uri: str, name: str = "") -> SpaceEntry:
        """Add a space, named name (or, if empty, after its key). One already added keeps its entry."""
        try:
            Ref.parse(uri)
        except ValueError as e:
            raise SettingsError(f"space {uri!r}: {e}") from None
        if name:
            _check_space_name(name)
        for e in self.spaces:
            if e.uri == uri:
                if name and name != e.name:
                    raise SettingsError(f'{uri} is already added, as "{e.name}"')
                return e
            if name and e.name == name:
                raise SettingsError(f'the name "{name}" is already used for {e.uri}')
        if not name:
            base = _space_key(uri)
            try:
                _check_space_name(base)
            except SettingsError:
                base = "space"
            name = base
            n = 2
            while self._entry(name) >= 0:
                name = f"{base}-{n}"
                n += 1
        e = SpaceEntry(name=name, uri=uri)
        self.spaces.append(e)
        if not self.default_space:
            self.default_space = name
        return e

    def use_space(self, name_or_uri: str) -> SpaceEntry:
        """Make a space the default, adding it first if it's a URI not yet added."""
        i = self._entry(name_or_uri)
        if i < 0:
            if not name_or_uri.startswith("at://"):
                raise SettingsError(f"no space named {name_or_uri!r}: use its at:// URI to add it")
            e = self.add_space(name_or_uri)
            self.default_space = e.name
            return e
        self.default_space = self.spaces[i].name
        return self.spaces[i]

    def remove_space(self, name_or_uri: str) -> None:
        i = self._entry(name_or_uri)
        if i < 0:
            raise SettingsError(f"no space {name_or_uri!r}")
        removed = self.spaces.pop(i)
        if self.default_space == removed.name:
            self.default_space = self.spaces[0].name if self.spaces else ""

    def default(self) -> SpaceEntry | None:
        i = self._entry(self.default_space) if self.default_space else -1
        if i >= 0:
            return self.spaces[i]
        return self.spaces[0] if self.spaces else None

    def resolve(self, name_or_uri: str = "") -> SpaceEntry:
        """Find a space by name or URI; "" is the default. A URI that isn't set up resolves too,
        named after its key, for one-off use."""
        if name_or_uri == "":
            e = self.default()
            if e is None:
                raise SettingsError(
                    "no memory space set up: add one with settings.add_space(uri), or make one with create_space"
                )
            return e
        i = self._entry(name_or_uri)
        if i >= 0:
            return self.spaces[i]
        if name_or_uri.startswith("at://"):
            try:
                Ref.parse(name_or_uri)
            except ValueError as e:
                raise SettingsError(f"space {name_or_uri!r}: {e}") from None
            return SpaceEntry(name=_space_key(name_or_uri), uri=name_or_uri)
        raise SettingsError(f"no space named {name_or_uri!r}")

    # ---- appview ----

    def set_appview(self, appview_url: str) -> None:
        """Point at another appview, whose DID follows from its host."""
        self.appview_url, self.appview_did = appview_url, ""
        self.apply_defaults()

    def apply_defaults(self) -> None:
        if not self.appview_url:
            self.appview_url = DEFAULT_APPVIEW_URL
        self.appview_url = self.appview_url.rstrip("/")
        if not self.appview_did:
            host = urlparse(self.appview_url).hostname
            if host:
                self.appview_did = "did:web:" + host

    # ---- checks ----

    def check(self) -> None:
        """Raise what's missing before the agent can run."""
        if not self.spaces:
            raise SettingsError("no memory space set up: add one with settings.add_space(uri)")
        for e in self.spaces:
            try:
                Ref.parse(e.uri)
            except ValueError as err:
                raise SettingsError(f"space {e.name!r}: {err}") from None
        self.check_account()

    def check_account(self) -> None:
        """Raise what's missing to sign in and reach the appview, whatever the spaces."""
        if not urlparse(self.appview_url).netloc:
            raise SettingsError(f"appview URL {self.appview_url!r} isn't a URL")
        a = self.account
        if a.sign_in == SIGN_IN_PASSWORD:
            if not a.handle or not a.password:
                raise SettingsError("password sign-in needs a handle and a password")
        elif a.sign_in == SIGN_IN_OAUTH:
            if not a.did or not a.session_id or not a.callback:
                raise SettingsError("not signed in: run `engram login`")
        else:
            raise SettingsError("not signed in: run `engram login` (or set ENGRAM_IDENTIFIER and ENGRAM_PASSWORD)")

    # ---- the file ----

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = dict(self.extra)
        if self.spaces:
            d["spaces"] = [e.to_dict() for e in self.spaces]
        if self.default_space:
            d["defaultSpace"] = self.default_space
        if self.appview_url:
            d["appviewUrl"] = self.appview_url
        if self.appview_did:
            d["appviewDid"] = self.appview_did
        d["account"] = self.account.to_dict()
        if e := self.embed.to_dict():
            d["embed"] = e
        return d

    def save(self, path: str | os.PathLike[str] | None = None) -> None:
        """Write the settings file, readable only by its owner."""
        p = Path(path) if path else config_path()
        p.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        raw = json.dumps(self.to_dict(), indent=2) + "\n"
        fd, tmp = tempfile.mkstemp(dir=p.parent, prefix=".config-")
        try:
            with os.fdopen(fd, "w") as f:
                f.write(raw)
            os.replace(tmp, p)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)

    def _apply_env(self, getenv: Callable[[str], str | None]) -> None:
        def get(k: str) -> str:
            return (getenv(k) or "").strip()

        if v := get("ENGRAM_SPACES"):
            self.spaces, self.default_space = [], ""
            for item in v.split(","):
                item = item.strip()
                if not item:
                    continue
                name, sep, uri = item.partition("=")
                if not sep:
                    name, uri = "", item
                try:
                    self.add_space(uri.strip(), name.strip())
                except SettingsError as e:
                    raise SettingsError(f"ENGRAM_SPACES: {e}") from None
        if v := get("ENGRAM_SPACE"):
            try:
                self.use_space(v)
            except SettingsError as e:
                raise SettingsError(f"ENGRAM_SPACE: {e}") from None
        if (v := get("ENGRAM_APPVIEW_URL")) and v != self.appview_url:
            # A different appview has its own DID.
            self.appview_url, self.appview_did = v, ""
        if v := get("ENGRAM_APPVIEW_DID"):
            self.appview_did = v
        ident, password = get("ENGRAM_IDENTIFIER"), get("ENGRAM_PASSWORD")
        if ident and password:
            self.account = Account(
                handle=ident, sign_in=SIGN_IN_PASSWORD, password=password, pds_host=get("ENGRAM_PDS_HOST")
            )
        for attr, key in (
            ("url", "ENGRAM_EMBED_URL"),
            ("api_key", "ENGRAM_EMBED_API_KEY"),
            ("model", "ENGRAM_EMBED_MODEL"),
            ("digest", "ENGRAM_EMBED_MODEL_DIGEST"),
            ("provider", "ENGRAM_EMBED_PROVIDER"),
        ):
            if v := get(key):
                setattr(self.embed, attr, v)


_KNOWN_KEYS = {"spaces", "defaultSpace", "space", "appviewUrl", "appviewDid", "account", "embed"}


def config_dir() -> Path:
    """Where the CLI keeps its settings and OAuth sessions: $ENGRAM_CONFIG_DIR, else the user's
    config directory's "engram"."""
    if d := os.environ.get("ENGRAM_CONFIG_DIR"):
        return Path(d)
    base = os.environ.get("XDG_CONFIG_HOME") or str(Path.home() / ".config")
    return Path(base) / "engram"


def config_path() -> Path:
    return config_dir() / "config.json"


def load_settings(
    path: str | os.PathLike[str] | None = None,
    getenv: Callable[[str], str | None] | None = os.environ.get,
) -> Settings:
    """Read the settings file (a missing one is empty) and apply ENGRAM_* variables from getenv
    (None for none)."""
    p = Path(path) if path else config_path()
    s = Settings()
    try:
        raw = p.read_text()
    except FileNotFoundError:
        raw = ""
    if raw.strip():
        try:
            d = json.loads(raw)
        except ValueError as e:
            raise SettingsError(f"{p}: {e}") from None
        s.spaces = [SpaceEntry(name=e.get("name", ""), uri=e.get("uri", "")) for e in d.get("spaces", []) or []]
        s.default_space = d.get("defaultSpace", "")
        s.appview_url = d.get("appviewUrl", "")
        s.appview_did = d.get("appviewDid", "")
        s.account = Account.from_dict(d.get("account", {}) or {})
        s.embed = Embed.from_dict(d.get("embed", {}) or {})
        s.extra = {k: v for k, v in d.items() if k not in _KNOWN_KEYS}
        # A file from before several spaces names one space.
        if legacy := d.get("space", ""):
            try:
                s.use_space(legacy)
            except SettingsError as e:
                raise SettingsError(f"{p}: {e}") from None
    if getenv is not None:
        s._apply_env(getenv)
    s.apply_defaults()
    return s
