"""Reads and writes ATProto Spaces as one account: exchanges the account's delegation tokens for
space credentials, and signs requests with the key each credential is bound to. Mirrors
internal/spaceclient."""

from __future__ import annotations

import asyncio
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Protocol

import httpx

from . import space as spacelib
from .errors import EngramError, XrpcError
from .identity import Directory
from .session import PdsSession, _params, read_error

#: How long before expiry a cached credential is replaced.
RENEW_BEFORE = 90.0


class Delegator(Protocol):
    """Mints delegation tokens: proof, from a user's PDS, that this client acts for that user."""

    async def delegation_token(self, space_uri: str) -> str: ...


class SessionDelegator:
    """Asks the session's own PDS for delegation tokens."""

    def __init__(self, session: PdsSession) -> None:
        self.session = session

    async def delegation_token(self, space_uri: str) -> str:
        try:
            tok = await self.session.get("com.atproto.space.getDelegationToken", {"space": space_uri})
        except XrpcError as e:
            raise EngramError(f"getDelegationToken: {e}") from e
        return tok["token"]


@dataclass
class _Credential:
    jwt: str
    exp: float


class Client:
    """Reads spaces for a user, and writes as one account when it has a session."""

    def __init__(
        self,
        session: PdsSession | None,
        directory: Directory,
        http: httpx.AsyncClient,
        delegator: Delegator | None = None,
    ) -> None:
        if delegator is None:
            if session is None:
                raise ValueError("a client needs a session or a delegator")
            delegator = SessionDelegator(session)
        #: Authenticated to the account's own PDS; None for a client that only reads.
        self.session = session
        self.delegator = delegator
        self.dir = directory
        self.http = http
        # Each client binds its credentials to its own fresh P-256 key.
        self._key = spacelib.generate_key()
        self._creds: dict[str, _Credential] = {}
        self._lock = asyncio.Lock()

    @property
    def did(self) -> str:
        """The account's DID, or "" for a client without a session."""
        return self.session.did if self.session else ""

    async def space_host(self, authority: str) -> str:
        """Where a space authority serves its spaces: its #atproto_space_host service, else its PDS."""
        ident = await self.dir.lookup_did(authority)
        host = ident.service_endpoint("atproto_space_host") or ident.pds_endpoint()
        if not host:
            raise EngramError(f"{authority} publishes no space host or PDS")
        return host

    async def pds_host(self, did: str) -> str:
        ident = await self.dir.lookup_did(did)
        host = ident.pds_endpoint()
        if not host:
            raise EngramError(f"{did} publishes no PDS")
        return host

    async def credential(self, space_uri: str) -> str:
        """A space credential, minting a new one when none is cached or it's close to expiry."""
        async with self._lock:
            cr = self._creds.get(space_uri)
            if cr and cr.exp - time.time() > RENEW_BEFORE:
                return cr.jwt
            return await self._refresh_credential(space_uri)

    def invalidate(self, space_uri: str) -> None:
        """Drop a cached credential, e.g. after a host rejects it."""
        self._creds.pop(space_uri, None)

    async def _refresh_credential(self, space_uri: str) -> str:
        ref = spacelib.Ref.parse(space_uri)
        token = await self.delegator.delegation_token(space_uri)
        host = await self.space_host(ref.authority)
        # The exchange signs only the authorization; the signature's key becomes the credential's
        # bound key.
        headers = spacelib.create_space_sig_headers(self._key, "Bearer " + token, "")
        try:
            out = await self._do(
                "POST", host, "com.atproto.space.getSpaceCredential", None, {"space": space_uri}, headers
            )
        except XrpcError as e:
            raise EngramError(f"getSpaceCredential: {e}") from e
        jwt = out["credential"]
        try:
            exp = float(spacelib.jwt_payload(jwt)["exp"])
        except (ValueError, KeyError, TypeError) as e:
            raise EngramError(f"getSpaceCredential returned a bad credential: {e}") from e
        self._creds[space_uri] = _Credential(jwt, exp)
        return jwt

    async def signed_headers(self, space_uri: str, audience: str) -> dict[str, str]:
        """The headers presenting a space credential to an audience DID."""
        cred = await self.credential(space_uri)
        return spacelib.create_space_sig_headers(self._key, "Atproto-Space " + cred, audience)

    async def query(
        self, host: str, space_uri: str, audience: str, nsid: str, params: dict[str, Any] | None = None
    ) -> Any:
        """A credentialed GET. On an auth failure it retries once with a fresh credential."""
        return await self._with_credential(space_uri, audience, lambda h: self._do("GET", host, nsid, params, None, h))

    async def procedure(self, host: str, space_uri: str, audience: str, nsid: str, body: Any = None) -> Any:
        """A credentialed POST."""
        return await self._with_credential(space_uri, audience, lambda h: self._do("POST", host, nsid, None, body, h))

    async def query_raw(
        self, host: str, space_uri: str, audience: str, nsid: str, params: dict[str, Any] | None = None
    ) -> bytes:
        """A credentialed GET returning the raw body, for binary responses such as getRepo's CAR."""

        async def go(h: dict[str, str]) -> bytes:
            resp = await self._send("GET", host, nsid, params, None, h)
            if resp.status_code != 200:
                raise read_error(resp)
            return resp.content

        return await self._with_credential(space_uri, audience, go)

    async def _with_credential(
        self, space_uri: str, audience: str, fn: Callable[[dict[str, str]], Awaitable[Any]]
    ) -> Any:
        for attempt in range(2):
            h = await self.signed_headers(space_uri, audience)
            try:
                return await fn(h)
            except XrpcError as e:
                if attempt == 0 and e.status == 401:
                    self.invalidate(space_uri)
                    continue
                raise
        raise AssertionError("unreachable")

    async def _send(
        self, method: str, host: str, nsid: str, params: dict[str, Any] | None, body: Any, headers: dict[str, str]
    ) -> httpx.Response:
        return await self.http.request(
            method,
            host.rstrip("/") + "/xrpc/" + nsid,
            params=_params(params),
            json=body if body is not None else None,
            headers=headers,
        )

    async def _do(
        self, method: str, host: str, nsid: str, params: dict[str, Any] | None, body: Any, headers: dict[str, str]
    ) -> Any:
        resp = await self._send(method, host, nsid, params, body, headers)
        if resp.status_code != 200:
            raise read_error(resp)
        if not resp.content:
            return None
        try:
            return resp.json()
        except ValueError:
            return None
