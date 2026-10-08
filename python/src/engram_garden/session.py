"""A signed-in session at the account's own PDS (password sign-in)."""

from __future__ import annotations

import asyncio
from typing import Any

import httpx

from .errors import SignInExpiredError, XrpcError


def read_error(resp: httpx.Response) -> XrpcError:
    """An XRPC error from a non-200 response."""
    name, message = "", ""
    try:
        body = resp.json()
        if isinstance(body, dict):
            name = str(body.get("error", ""))
            message = str(body.get("message", ""))
    except ValueError:
        pass
    if not name:
        name = resp.reason_phrase or str(resp.status_code)
        message = resp.text[: 64 << 10].strip()
    return XrpcError(resp.status_code, name, message)


class PdsSession:
    """Authenticated to one account's PDS. Refreshes its tokens when they expire."""

    def __init__(self, http: httpx.AsyncClient, pds: str, did: str, handle: str, access_jwt: str, refresh_jwt: str):
        self.http = http
        self.pds = pds.rstrip("/")
        self.did = did
        self.handle = handle
        self._access = access_jwt
        self._refresh = refresh_jwt
        self._lock = asyncio.Lock()

    @classmethod
    async def login(cls, http: httpx.AsyncClient, pds: str, identifier: str, password: str) -> PdsSession:
        """Sign in with a handle (or DID) and password (an app password works)."""
        resp = await http.post(
            f"{pds.rstrip('/')}/xrpc/com.atproto.server.createSession",
            json={"identifier": identifier, "password": password},
        )
        if resp.status_code != 200:
            raise read_error(resp)
        d = resp.json()
        return cls(http, pds, d["did"], d.get("handle", identifier), d["accessJwt"], d["refreshJwt"])

    async def _renew(self, stale: str) -> None:
        async with self._lock:
            if self._access != stale:
                return  # another request already refreshed
            resp = await self.http.post(
                f"{self.pds}/xrpc/com.atproto.server.refreshSession",
                headers={"Authorization": f"Bearer {self._refresh}"},
            )
            if resp.status_code != 200:
                raise SignInExpiredError(f"token refresh failed: {read_error(resp)}")
            d = resp.json()
            self._access, self._refresh = d["accessJwt"], d["refreshJwt"]

    async def _send(self, method: str, nsid: str, params: dict[str, Any] | None, body: Any) -> httpx.Response:
        for attempt in range(2):
            access = self._access
            resp = await self.http.request(
                method,
                f"{self.pds}/xrpc/{nsid}",
                params=_params(params),
                json=body if method == "POST" else None,
                headers={"Authorization": f"Bearer {access}"},
            )
            if attempt == 0 and resp.status_code in (400, 401) and _expired(resp):
                await self._renew(access)
                continue
            return resp
        raise AssertionError("unreachable")

    async def get(self, nsid: str, params: dict[str, Any] | None = None) -> Any:
        """An authenticated query."""
        resp = await self._send("GET", nsid, params, None)
        if resp.status_code != 200:
            raise read_error(resp)
        return resp.json()

    async def post(self, nsid: str, body: Any = None) -> Any:
        """An authenticated procedure. Returns the JSON response, or None if it has no body."""
        resp = await self._send("POST", nsid, None, body if body is not None else {})
        if resp.status_code != 200:
            raise read_error(resp)
        if not resp.content:
            return None
        try:
            return resp.json()
        except ValueError:
            return None


def _expired(resp: httpx.Response) -> bool:
    try:
        return resp.json().get("error") == "ExpiredToken"
    except (ValueError, AttributeError):
        return False


def _params(params: dict[str, Any] | None) -> dict[str, Any] | None:
    if not params:
        return None
    out: dict[str, Any] = {}
    for k, v in params.items():
        if isinstance(v, bool):
            out[k] = "true" if v else "false"
        elif isinstance(v, (list, tuple)):
            out[k] = [str(x) for x in v]
        elif v is not None:
            out[k] = str(v)
    return out
