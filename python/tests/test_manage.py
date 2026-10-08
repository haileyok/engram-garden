"""Running spaces: create, members, model, indexing."""

from __future__ import annotations

from urllib.parse import parse_qs, urlparse

import pytest

import engram_garden as eg
from engram_garden import lex

from .conftest import make_settings
from .fakes import AGENT_DID, APPVIEW_URL, AUTHORITY_DID, OTHER_DID, SPACE, World


async def test_create_space_declares_the_model_and_becomes_usable(world: World, http):
    s = make_settings()
    s.spaces, s.default_space = [], ""
    saved: list[eg.Settings] = []
    async with await eg.open_spaces(s, http=http) as sp:
        sp.save = saved.append
        out = await sp.create_space("mine", model="hashing-64", dims=64)
        assert out.space.name == "mine"
        assert out.space.uri == f"at://{AGENT_DID}/space/garden.engram.space/mine"
        assert out.model is not None and out.model.dims == 64
        assert "Add members" in out.next
        body = world.spaces_created[0]
        assert body["spaceType"] == "garden.engram.space" and body["skey"] == "mine"
        assert body["readPolicy"] == {"$type": "com.atproto.simplespace.defs#memberListPolicy"}
        sp.save_settings()
        assert saved[0].default_space == "mine"
        assert [e.uri for e in saved[0].spaces] == [out.space.uri]
        # The new space is the default and works end to end.
        r = await sp.remember("first memory in my own space")
        assert r.uri.startswith(out.space.uri + "/")
        found = await sp.recall("first memory")
        assert found.memories[0].text == "first memory in my own space"


async def test_create_space_without_a_model_says_what_is_next(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as sp:
        out = await sp.create_space("bare")
        assert out.model is None and "Declare its embedding model" in out.next


async def test_create_space_rejects_bad_names(spaces: eg.Spaces):
    for bad in ("", "has space", "a/b", ".."):
        with pytest.raises(eg.EngramError, match="names use"):
            await spaces.create_space(bad)


async def test_create_space_failure_to_declare_is_reported_not_raised(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as sp:
        out = await sp.create_space("half", model="hashing-64")  # hashing needs dims
        assert out.model is None and "declaring its model failed" in out.next


async def test_only_the_authority_manages(spaces: eg.Spaces):
    with pytest.raises(eg.EngramError, match="only the space's authority"):
        await spaces.list_members()
    with pytest.raises(eg.EngramError, match="only the space's authority"):
        await spaces.add_member("other.test")
    with pytest.raises(eg.EngramError, match="only the space's authority"):
        await spaces.set_model(action="cancel")
    with pytest.raises(eg.EngramError, match="only the space's authority"):
        await spaces.index_space()


async def test_members(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as sp:
        out = await sp.create_space("team", model="hashing-64", dims=64)
        uri = out.space.uri
        assert [m.did for m in (await sp.list_members(space=uri)).members] == [AGENT_DID]
        added = await sp.add_member("@other.test", space=uri)
        assert added.member.did == OTHER_DID and added.member.write is True
        ro = await sp.add_member(AUTHORITY_DID, space=uri, read_only=True)
        assert (ro.member.read, ro.member.write) == (True, False)
        members = {m.did: m for m in (await sp.list_members(space=uri)).members}
        assert set(members) == {AGENT_DID, OTHER_DID, AUTHORITY_DID}
        assert members[OTHER_DID].handle == "other.test"
        assert members[AUTHORITY_DID].write is False
        removed = await sp.remove_member("other.test", space=uri)
        assert removed.member.did == OTHER_DID
        assert OTHER_DID not in {m.did for m in (await sp.list_members(space=uri)).members}
        with pytest.raises(eg.EngramError, match="isn't a handle or DID|couldn't resolve"):
            await sp.add_member("not a handle", space=uri)


async def test_set_model_declare_next_promote_cancel(world: World, http):
    world.config = None  # a space with no model yet
    async with await eg.open_spaces(make_settings(), http=http) as sp:
        uri = (await sp.create_space("m")).space.uri
        with pytest.raises(eg.EngramError, match="declare a model first"):
            await sp.set_model(action="next", space=uri, model="hashing-32", dims=32)
        out = await sp.set_model(action="declare", space=uri, model="hashing-64", dims=64)
        assert out.model == lex.ModelInfo("hashing-64", "sha256:hashing", 64)
        assert world.config["dims"] == 64 and world.config["$type"] == lex.CONFIG_COLLECTION
        nxt = await sp.set_model(action="next", space=uri, model="hashing-32", dims=32)
        assert nxt.next_model == lex.ModelInfo("hashing-32", "sha256:hashing", 32)
        assert (await sp.model(space=uri)).next_model.dims == 32
        cancelled = await sp.set_model(action="cancel", space=uri)
        assert cancelled.next_model is None
        await sp.set_model(action="next", space=uri, model="hashing-32", dims=32)
        promoted = await sp.set_model(action="promote", space=uri)
        assert promoted.model.dims == 32 and promoted.next_model is None
        with pytest.raises(eg.EngramError, match="no model change in progress"):
            await sp.set_model(action="promote", space=uri)
        with pytest.raises(eg.EngramError, match="action must be"):
            await sp.set_model(action="wat", space=uri)


async def test_nomic_gets_its_task_prefixes():
    cfg = lex.change_config(None, lex.DECLARE, lex.ModelInfo("nomic-embed-text", "sha256:ab", 768))
    assert (cfg.document_prefix, cfg.query_prefix) == ("search_document: ", "search_query: ")
    cfg = lex.change_config(None, lex.DECLARE, lex.ModelInfo("other", "sha256:ab", 8))
    assert (cfg.document_prefix, cfg.query_prefix) == ("", "")


async def test_index_space_gives_a_link_until_granted(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as sp:
        uri = (await sp.create_space("idx")).space.uri
        world.access_state = "missing"
        out = await sp.index_space(space=uri)
        assert out.state == "missing"
        u = urlparse(out.link)
        assert out.link.startswith(APPVIEW_URL + "/oauth/grant?")
        assert parse_qs(u.query) == {"space": [uri], "mode": ["grant"]}
        assert AGENT_DID in out.note
        world.access_state = "granted"
        done = await sp.index_space(space=uri)
        assert done.state == "granted" and done.link == ""
        stop = await sp.index_space(space=uri, stop=True)
        assert parse_qs(urlparse(stop.link).query)["mode"] == ["stop"]
        assert await sp.index_state(uri) == "granted"


async def test_space_listing_includes_new_space_after_create(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as sp:
        uri = (await sp.create_space("listed")).space.uri
        uris = {i.uri for i in (await sp.list_spaces()).spaces}
        assert {SPACE, uri} <= uris
