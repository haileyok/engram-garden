"""Resolving DIDs and handles: where an account's PDS and a space authority's space host are."""

from __future__ import annotations

import re
import time
from dataclasses import dataclass, field
from typing import Any

import httpx

from .errors import EngramError

PLC_DIRECTORY = "https://plc.directory"
#: Only used to resolve a handle whose own /.well-known/atproto-did lookup fails.
PUBLIC_APPVIEW = "https://public.api.bsky.app"

_HANDLE_RE = re.compile(r"^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$")


@dataclass
class Identity:
    """A resolved DID document."""

    did: str
    doc: dict[str, Any]
    handle: str = ""

    def service_endpoint(self, service_id: str) -> str:
        """The endpoint of the service with this fragment id (e.g. atproto_pds), or ""."""
        for svc in self.doc.get("service", []) or []:
            sid = svc.get("id", "")
            if sid in (f"#{service_id}", f"{self.did}#{service_id}"):
                ep = svc.get("serviceEndpoint")
                if isinstance(ep, str):
                    return ep.rstrip("/")
        return ""

    def pds_endpoint(self) -> str:
        return self.service_endpoint("atproto_pds")


@dataclass
class Directory:
    """Resolves DIDs (did:plc and did:web) and handles, caching DID documents briefly."""

    http: httpx.AsyncClient
    plc_url: str = PLC_DIRECTORY
    public_appview: str = PUBLIC_APPVIEW
    ttl: float = 300.0
    _cache: dict[str, tuple[float, Identity]] = field(default_factory=dict)

    async def lookup_did(self, did: str, *, fresh: bool = False) -> Identity:
        hit = self._cache.get(did)
        if hit and not fresh and time.monotonic() - hit[0] < self.ttl:
            return hit[1]
        if did.startswith("did:plc:"):
            url = f"{self.plc_url.rstrip('/')}/{did}"
        elif did.startswith("did:web:"):
            host = did.removeprefix("did:web:").replace(":", "/").replace("%3A", ":")
            url = f"https://{host}/.well-known/did.json" if "/" not in host else f"https://{host}/did.json"
        else:
            raise EngramError(f"unsupported DID method: {did}")
        resp = await self.http.get(url)
        if resp.status_code != 200:
            raise EngramError(f"resolving {did}: {resp.status_code}")
        doc = resp.json()
        handle = ""
        for aka in doc.get("alsoKnownAs", []) or []:
            if isinstance(aka, str) and aka.startswith("at://"):
                handle = aka.removeprefix("at://")
                break
        ident = Identity(did=did, doc=doc, handle=handle)
        self._cache[did] = (time.monotonic(), ident)
        return ident

    async def resolve_handle(self, handle: str) -> str:
        """The DID a handle points to."""
        handle = handle.lower()
        if not _HANDLE_RE.match(handle):
            raise EngramError(f"{handle!r} isn't a handle")
        try:
            resp = await self.http.get(f"https://{handle}/.well-known/atproto-did")
            if resp.status_code == 200 and resp.text.strip().startswith("did:"):
                return resp.text.strip()
        except httpx.HTTPError:
            pass
        resp = await self.http.get(
            f"{self.public_appview.rstrip('/')}/xrpc/com.atproto.identity.resolveHandle",
            params={"handle": handle},
        )
        if resp.status_code != 200:
            raise EngramError(f"couldn't resolve {handle}: {resp.status_code}")
        return resp.json()["did"]

    async def lookup(self, identifier: str) -> Identity:
        """Resolve a handle or DID."""
        did = identifier if identifier.startswith("did:") else await self.resolve_handle(identifier)
        return await self.lookup_did(did)
