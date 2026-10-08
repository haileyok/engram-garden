from __future__ import annotations

import httpx
import pytest

import engram_garden as eg

from .fakes import AGENT_DID, APPVIEW_DID, APPVIEW_URL, PASSWORD, PDS_URL, SPACE, World


def make_settings(**kw) -> eg.Settings:
    s = eg.Settings(
        spaces=[eg.SpaceEntry("memory", SPACE)],
        default_space="memory",
        appview_url=APPVIEW_URL,
        appview_did=APPVIEW_DID,
        account=eg.Account(handle=AGENT_DID, sign_in=eg.SIGN_IN_PASSWORD, password=PASSWORD),
        embed=eg.Embed(provider="hashing"),
        **kw,
    )
    s.apply_defaults()
    return s


@pytest.fixture
def world() -> World:
    return World()


@pytest.fixture
def http(world: World):
    return httpx.AsyncClient(transport=world.transport, timeout=10)


@pytest.fixture
async def spaces(world: World, http: httpx.AsyncClient):
    async with await eg.open_spaces(make_settings(), http=http) as s:
        yield s


__all__ = ["PDS_URL", "make_settings"]
