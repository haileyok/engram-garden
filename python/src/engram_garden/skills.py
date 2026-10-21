"""Skills: instructions for a kind of work in the Agent Skills format, kept in members' own repos in a space,
keyed by name. They aren't indexed: an agent reads every member's skills directly from their PDS with the
space credential. For each reader, its own version of a name wins, then the authority's, then the most
recently updated. Mirrors internal/agent/skills.go and internal/lex/skill.go."""

from __future__ import annotations

import asyncio
import posixpath
import re
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import TYPE_CHECKING, Any

from . import lex
from .errors import EngramError, XrpcError, is_error
from .space import Ref

if TYPE_CHECKING:
    from .agent import Agent

SKILL_COLLECTION = lex.SKILL_COLLECTION
MAX_NAME = 64
MAX_DESCRIPTION = 1024
MAX_INSTRUCTIONS = 200_000
MAX_FILES = 32
MAX_FILE_PATH = 512
MAX_FILE_CONTENT = 200_000
MAX_NOTE = 2000
MAX_BYTES = 500_000

_NAME = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")


_RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")


def parse_time(v: str) -> datetime | None:
    """An RFC 3339 time (as Go's time.RFC3339Nano reads it), or None."""
    if not isinstance(v, str) or not _RFC3339.match(v):
        return None
    s = v[:-1] + "+00:00" if v.endswith("Z") else v
    head, _, rest = s.partition(".")
    if rest:  # fromisoformat wants at most 6 fractional digits
        digits = re.match(r"\d+", rest).group(0)  # type: ignore[union-attr]
        s = f"{head}.{digits[:6].ljust(6, '0')}{rest[len(digits) :]}"
    try:
        return datetime.fromisoformat(s)
    except ValueError:
        return None


def valid_skill_name(name: str) -> bool:
    """Lowercase letters, digits and single hyphens between them, at most 64 characters."""
    return len(name) <= MAX_NAME and bool(_NAME.match(name))


def check_file_path(p: str) -> None:
    """A skill file's path: relative, forward slashes, inside the skill's directory, not SKILL.md."""
    if not p or len(p) > MAX_FILE_PATH:
        raise EngramError(f"file path {p!r}: empty or too long")
    if "\\" in p or p.startswith("/") or "\x00" in p:
        raise EngramError(f"file path {p!r}: use a relative path with forward slashes")
    if posixpath.normpath(p) != p:
        raise EngramError(f"file path {p!r}: not in its simplest form")
    if p == ".." or p.startswith("../"):
        raise EngramError(f"file path {p!r}: outside the skill's directory")
    if p == "SKILL.md":
        raise EngramError("SKILL.md is the skill's instructions, not one of its files")
    # Names a skill directory never picks up when read back, so a record never holds a file that editing the
    # directory would silently drop.
    for seg in p.split("/"):
        if seg.startswith(".") or seg in ("node_modules", "__pycache__"):
            raise EngramError(f"file path {p!r}: dotfiles, node_modules and __pycache__ aren't kept in skills")
        if seg == "SKILL.md":  # agents find skills by their SKILL.md, at any depth
            raise EngramError(f"file path {p!r}: a SKILL.md inside a skill would be a skill of its own")


@dataclass
class SkillFile:
    path: str
    content: str
    executable: bool = False

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"path": self.path, "content": self.content}
        if self.executable:
            d["executable"] = True
        return d

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> SkillFile:
        return cls(str(d.get("path", "")), str(d.get("content", "")), bool(d.get("executable", False)))


@dataclass
class Skill:
    """A garden.engram.skill record."""

    name: str
    description: str
    instructions: str
    files: list[SkillFile] = field(default_factory=list)
    disable_model_invocation: bool = False
    note: str = ""
    created_at: str = ""
    updated_at: str = ""

    def validate(self) -> None:
        if not valid_skill_name(self.name):
            raise EngramError(
                f"skill name {self.name!r}: use lowercase letters, digits and single hyphens "
                f"(at most {MAX_NAME} characters)"
            )
        if not self.description.strip():
            raise EngramError("a skill needs a description: what it does and when to use it")
        if len(self.description.encode()) > MAX_DESCRIPTION:
            raise EngramError(f"description is too long (at most {MAX_DESCRIPTION} bytes)")
        if not self.instructions.strip():
            raise EngramError("a skill needs instructions")
        if len(self.instructions.encode()) > MAX_INSTRUCTIONS:
            raise EngramError(f"instructions are too long (at most {MAX_INSTRUCTIONS} bytes)")
        if len(self.note.encode()) > MAX_NOTE:
            raise EngramError(f"note is too long (at most {MAX_NOTE} bytes)")
        for label, v in (("createdAt", self.created_at), ("updatedAt", self.updated_at)):
            if parse_time(v) is None:
                raise EngramError(f"{label} {v!r} isn't an RFC 3339 time")
        if len(self.files) > MAX_FILES:
            raise EngramError(f"at most {MAX_FILES} files")
        total = len(self.description.encode()) + len(self.instructions.encode()) + len(self.note.encode())
        seen: set[str] = set()
        for f in self.files:
            check_file_path(f.path)
            if f.path in seen:
                raise EngramError(f"file {f.path} is listed twice")
            seen.add(f.path)
            if "\x00" in f.content:
                raise EngramError(f"file {f.path} isn't text")
            if len(f.content.encode()) > MAX_FILE_CONTENT:
                raise EngramError(f"file {f.path} is too long (at most {MAX_FILE_CONTENT} bytes)")
            total += len(f.path.encode()) + len(f.content.encode())
        if total > MAX_BYTES:
            raise EngramError(f"the skill is too big ({total} bytes; at most {MAX_BYTES} in all)")

    def record(self) -> dict[str, Any]:
        rec: dict[str, Any] = {
            "$type": SKILL_COLLECTION,
            "name": self.name,
            "description": self.description,
            "instructions": self.instructions,
            "createdAt": self.created_at,
            "updatedAt": self.updated_at,
        }
        if self.files:
            rec["files"] = [f.to_dict() for f in self.files]
        if self.disable_model_invocation:
            rec["disableModelInvocation"] = True
        if self.note:
            rec["note"] = self.note
        return rec

    @classmethod
    def from_record(cls, d: dict[str, Any]) -> Skill:
        """From a record. Raises EngramError for one that isn't shaped like a skill, as Go's decoding fails."""
        files = d.get("files")
        if files is None:
            files = []
        if not isinstance(files, list) or not all(isinstance(f, dict) for f in files):
            raise EngramError("not a skill record: files isn't a list of files")
        for k in ("name", "description", "instructions", "note", "createdAt", "updatedAt"):
            if d.get(k) is not None and not isinstance(d[k], str):
                raise EngramError(f"not a skill record: {k} isn't a string")
        if d.get("disableModelInvocation") is not None and not isinstance(d["disableModelInvocation"], bool):
            raise EngramError("not a skill record: disableModelInvocation isn't a boolean")
        for f in files:
            if not isinstance(f.get("path", ""), str) or not isinstance(f.get("content", ""), str):
                raise EngramError("not a skill record: a file's path or content isn't a string")
            if f.get("executable") is not None and not isinstance(f["executable"], bool):
                raise EngramError("not a skill record: a file's executable isn't a boolean")
        return cls(
            name=str(d.get("name", "")),
            description=str(d.get("description", "")),
            instructions=str(d.get("instructions", "")),
            files=[SkillFile.from_dict(f) for f in files],
            disable_model_invocation=bool(d.get("disableModelInvocation", False)),
            note=str(d.get("note", "") or ""),
            created_at=str(d.get("createdAt", "") or ""),
            updated_at=str(d.get("updatedAt", "") or ""),
        )


@dataclass
class SkillInfo:
    """A skill as an agent sees it: the record, where it is, and the versions of the name it wins over."""

    skill: Skill
    uri: str
    cid: str
    author: str
    space: str = ""
    shadowed: list[str] = field(default_factory=list)

    @property
    def name(self) -> str:
        return self.skill.name

    def to_dict(self, content: bool = True) -> dict[str, Any]:
        d = self.skill.record()
        d.pop("$type", None)
        if not content:
            d.pop("instructions", None)
            d.pop("files", None)
        d.update({"uri": self.uri, "cid": self.cid, "author": self.author})
        if self.space:
            d["space"] = self.space
        if self.shadowed:
            d["shadowed"] = self.shadowed
        return d


@dataclass
class SaveSkillOut:
    uri: str
    cid: str
    created: bool
    note: str = ""
    #: The name of the space it was saved in.
    space: str = ""


def _now() -> str:
    return lex.format_time(datetime.now(UTC))


def resolve(all_skills: list[SkillInfo], me: str, authority: str) -> list[SkillInfo]:
    """One version of each name: the reader's own, then the authority's, then the most recently updated."""

    def rank(s: SkillInfo) -> int:
        return 0 if s.author == me else 1 if s.author == authority else 2

    limit = datetime.now(UTC) + timedelta(hours=1)
    oldest = datetime.min.replace(tzinfo=UTC)

    def when(s: SkillInfo) -> datetime:
        t = parse_time(s.skill.updated_at)
        return oldest if t is None or t > limit else t

    by_name: dict[str, list[SkillInfo]] = {}
    for s in all_skills:
        by_name.setdefault(s.name, []).append(s)
    out = []
    for versions in by_name.values():
        # By URI, then newest first by instant (a time in the future counts as oldest), then (stable) by
        # rank: Go's order.
        versions.sort(key=lambda s: s.uri)
        versions.sort(key=when, reverse=True)
        versions.sort(key=rank)
        win = versions[0]
        win.shadowed = [v.uri for v in versions[1:]]
        out.append(win)
    out.sort(key=lambda s: s.name)
    return out


def _not_found(e: XrpcError) -> bool:
    msg = str(e)
    return is_error(e, "RecordNotFound", "RepoNotFound", "NotFound") or any(
        s in msg for s in ("RecordNotFound", "RepoNotFound", "Could not locate record")
    )


async def members(a: Agent) -> list[str]:
    """The DIDs with a repo in the space, from its authority."""
    ref = Ref.parse(a.space)
    host = await a.client.space_host(ref.authority)
    dids: list[str] = []
    cursor = ""
    while True:
        params: dict[str, Any] = {"space": a.space, "limit": 100}
        if cursor:
            params["cursor"] = cursor
        try:
            out = await a.client.query(host, a.space, ref.authority, "com.atproto.space.listRepos", params) or {}
        except XrpcError as e:
            raise EngramError(f"listing the space's members: {e}") from e
        repos = out.get("repos") or []
        dids += [r["did"] for r in repos if r.get("did")]
        cursor = out.get("cursor") or ""
        if not cursor or not repos:
            return dids


async def list_records(a: Agent, did: str, collection: str) -> list[tuple[str, str, str, dict[str, Any]]]:
    """A member's records in a collection, from its PDS, as (uri, cid, rkey, value)."""
    ref = Ref.parse(a.space)
    host = await a.client.pds_host(did)
    recs = []
    cursor = ""
    while True:
        params: dict[str, Any] = {"space": a.space, "repo": did, "collection": collection, "limit": 100}
        if cursor:
            params["cursor"] = cursor
        try:
            out = await a.client.query(host, a.space, did, "com.atproto.space.listRecords", params) or {}
        except XrpcError as e:
            if _not_found(e):
                return recs
            raise
        items = out.get("records") or []
        for r in items:
            if r.get("collection") and r["collection"] != collection:
                continue
            rkey = r.get("rkey", "")
            recs.append((ref.record_uri(did, collection, rkey), r.get("cid", ""), rkey, r.get("value") or {}))
        cursor = out.get("cursor") or ""
        if not cursor or not items:
            return recs


async def skills(a: Agent) -> tuple[list[SkillInfo], list[str]]:
    """The skills in the agent's space, one version per name, and the members whose skills couldn't be read
    (so the list may lack a version)."""
    dids = await members(a)
    me = a.client.did
    if me not in dids:
        dids.append(me)
    results = await asyncio.gather(*(list_records(a, d, SKILL_COLLECTION) for d in dids), return_exceptions=True)
    found: list[SkillInfo] = []
    errors: list[BaseException] = []
    unreadable: list[str] = []
    for did, res in zip(dids, results, strict=True):
        if isinstance(res, BaseException):
            errors.append(res)
            unreadable.append(did)
            continue
        for uri, cid, rkey, value in res:
            try:
                s = Skill.from_record(value)
                if s.name != rkey:
                    continue
                s.validate()
            except EngramError:
                continue  # not a skill this agent can use
            found.append(SkillInfo(skill=s, uri=uri, cid=cid, author=did))
    if errors and not found:
        raise EngramError("; ".join(str(e) for e in errors))
    return resolve(found, me, Ref.parse(a.space).authority), sorted(unreadable)


async def own_skill(a: Agent, name: str) -> Skill | None:
    params = {"space": a.space, "repo": a.client.did, "collection": SKILL_COLLECTION, "rkey": name}
    try:
        out = await a.client.session.get("com.atproto.space.getRecord", params)
    except XrpcError as e:
        if _not_found(e):
            return None
        raise EngramError(f"reading your version of {name}: {e}") from e
    return Skill.from_record((out or {}).get("value") or {})


def _scope_hint(e: XrpcError, what: str) -> str:
    msg = str(e)
    if e.status in (401, 403) or "scope" in msg or "Forbidden" in msg:
        return f"{msg} (if your sign-in predates {what}, sign in again to allow writing them)"
    return msg


async def save_skill(
    a: Agent,
    name: str,
    description: str,
    instructions: str,
    files: list[SkillFile] | None = None,
    disable_model_invocation: bool | None = None,
    note: str = "",
) -> SaveSkillOut:
    """Write the agent's own version of a skill, keeping the current version's files and createdAt when the
    input leaves them out."""
    now = _now()
    s = Skill(
        name=name.strip(),
        description=description.strip(),
        instructions=instructions.strip() + "\n",
        files=list(files) if files is not None else [],
        note=note.strip(),
        created_at=now,
        updated_at=now,
    )
    if not valid_skill_name(s.name):
        s.validate()
    cur = await own_skill(a, s.name)
    if cur is not None:
        if cur.created_at:
            s.created_at = cur.created_at
        if files is None:
            s.files = cur.files
        s.disable_model_invocation = cur.disable_model_invocation
    if disable_model_invocation is not None:
        s.disable_model_invocation = disable_model_invocation
    s.validate()
    body = {
        "space": a.space,
        "repo": a.client.did,
        "collection": SKILL_COLLECTION,
        "rkey": s.name,
        "record": s.record(),
    }
    try:
        out = await a.client.session.post("com.atproto.space.putRecord", body) or {}
    except XrpcError as e:
        raise EngramError(f"saving skill {s.name}: {_scope_hint(e, 'skills')}") from e
    return SaveSkillOut(uri=out.get("uri", ""), cid=out.get("cid", ""), created=cur is None)


async def delete_skill(a: Agent, name: str) -> str:
    """Delete the agent's own version of a skill; returns its URI."""
    if not valid_skill_name(name):
        raise EngramError(f"{name!r} isn't a skill name")
    if await own_skill(a, name) is None:
        raise EngramError(f"you have no skill {name} in this space; you can only delete your own")
    me = a.client.did
    body = {"space": a.space, "repo": me, "collection": SKILL_COLLECTION, "rkey": name}
    try:
        await a.client.session.post("com.atproto.space.deleteRecord", body)
    except XrpcError as e:
        raise EngramError(f"deleting skill {name}: {_scope_hint(e, 'skills')}") from e
    return Ref.parse(a.space).record_uri(me, SKILL_COLLECTION, name)
