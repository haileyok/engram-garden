"""Several spaces, list_spaces, and keeping memories embedded with the space's model."""

from __future__ import annotations

import pytest

import engram_garden as eg
from engram_garden import lex

from .conftest import make_settings
from .fakes import AGENT_DID, APPVIEW_DID, AUTHORITY_DID, MODEL, SPACE, World

SPACE2 = f"at://{AUTHORITY_DID}/space/garden.engram.space/runbooks"
SPACE3 = f"at://{AUTHORITY_DID}/space/garden.engram.space/private"


@pytest.fixture
def two(world: World):
    world.accounts[AGENT_DID].member_of[SPACE2] = (True, True)
    return world


async def open_two(world: World, http, *extra: str) -> eg.Spaces:
    s = make_settings()
    s.add_space(SPACE2, "runbooks")
    for uri in extra:
        s.add_space(uri)
    return await eg.open_spaces(s, http=http)


async def test_remember_goes_to_the_default_or_the_named_space(two: World, http):
    async with await open_two(two, http) as s:
        a = await s.remember("in the default")
        b = await s.remember("in runbooks", space="runbooks")
        assert a.uri.startswith(SPACE + "/")
        assert b.uri.startswith(SPACE2 + "/")
        c = await s.remember("by uri", space=SPACE2)
        assert c.uri.startswith(SPACE2 + "/")
        with pytest.raises(eg.SettingsError, match="no space named"):
            await s.remember("nowhere", space="nope")


async def test_recall_searches_every_space_and_labels_them(two: World, http):
    two.add_other_memory("restart the database with care", space_uri=SPACE)
    two.add_other_memory("restart the database runbook step one", space_uri=SPACE2)
    async with await open_two(two, http) as s:
        out = await s.recall("restart the database", limit=10)
        by_space = {m.space for m in out.memories}
        assert by_space == {"memory", "runbooks"}
        sims = [m.similarity for m in out.memories]
        assert sims == sorted(sims, reverse=True)
        only = await s.recall("restart the database", space="runbooks")
        assert {m.space for m in only.memories} == {"runbooks"}
        capped = await s.recall("restart the database", limit=1)
        assert len(capped.memories) == 1
        everything = await s.recall("restart the database", space=eg.ALL_SPACES)
        assert len(everything.memories) == 2


async def test_recall_survives_one_space_failing(two: World, http):
    two.add_other_memory("restart the database with care", space_uri=SPACE)
    async with await open_two(two, http, SPACE3) as s:  # the agent isn't a member of SPACE3
        out = await s.recall("restart the database")
        assert [m.space for m in out.memories] == ["memory"]
        assert "Couldn't search private" in out.note


async def test_recall_fails_when_every_space_fails(world: World, http):
    s = make_settings()
    s.spaces = [eg.SpaceEntry("private", SPACE3)]
    s.default_space = "private"
    async with await eg.open_spaces(s, http=http) as sp:
        with pytest.raises(eg.EngramError, match="NotAMember|not a member"):
            await sp.recall("anything")


async def test_get_and_forget_find_the_space_from_the_uri(two: World, http):
    async with await open_two(two, http) as s:
        out = await s.remember("a runbook", space="runbooks")
        got = await s.get(out.uri)
        assert got.memory.space == "runbooks"
        await s.forget(out.uri)
        assert out.uri not in two.memories(SPACE2)


async def test_list_one_space_only(two: World, http):
    async with await open_two(two, http) as s:
        await s.remember("x")
        with pytest.raises(eg.EngramError, match="one space at a time"):
            await s.list(space=eg.ALL_SPACES)
        assert (await s.list(space="runbooks")).memories == []


async def test_list_spaces_describes_each_and_finds_others(two: World, http):
    two.accounts[AGENT_DID].member_of[SPACE3] = (True, True)  # a member, but not set up
    two.access_state = "missing"
    async with await open_two(two, http) as s:
        out = await s.list_spaces()
        by_name = {i.name: i for i in out.spaces}
        assert set(by_name) == {"memory", "runbooks", "private"}
        mem = by_name["memory"]
        assert mem.set_up and mem.default
        assert mem.model == MODEL
        assert mem.local_model == "ready"
        assert mem.indexing == "missing"
        assert "can't read this space" in mem.warning
        assert not by_name["runbooks"].default
        assert not by_name["private"].set_up
        assert out.note == ""


async def test_list_spaces_reports_a_local_model_problem(world: World, http):
    world.config = lex.Config(lex.ModelInfo("hashing-64", "sha256:other", 64)).record()
    async with await eg.open_spaces(make_settings(), http=http) as s:
        (info,) = (await s.list_spaces()).spaces
        assert "digest" in info.local_model


async def test_reembed_adds_vectors_for_the_next_model(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as s:
        await s.remember("one")
        await s.remember("two")
        agent, _ = s.agent()
        assert await agent.reembed() == 0
        nxt = lex.ModelInfo("hashing-32", MODEL.model_digest, 32)
        world.config = lex.Config(MODEL, next=nxt).record()
        assert await agent.reembed() == 2
        for rec in world.memories(SPACE, AGENT_DID).values():
            assert {e.key() for e in lex.parse_embeddings(rec)} == {MODEL.key(), nxt.key()}
        assert await agent.reembed() == 0
        # A memory written during the change carries both vectors.
        out = await s.remember("three")
        keys = {e.key() for e in lex.parse_embeddings(world.memories(SPACE)[out.uri])}
        assert keys == {MODEL.key(), nxt.key()}


async def test_reembed_pages_through_many_memories(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as s:
        for i in range(5):
            await s.remember(f"memory number {i}")
        # Strip every vector, as if written by an agent that never had the model.
        for rec in world.memories(SPACE, AGENT_DID).values():
            del rec["embedding"]
        agent, _ = s.agent()
        assert await agent.reembed() == 5
        assert all(lex.parse_embeddings(r) for r in world.memories(SPACE, AGENT_DID).values())


async def test_warm_and_indexing_use_the_appview_audience(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as s:
        agent, _ = s.agent()
        await agent.warm()
        assert await agent.indexing(fresh=True) == "granted"
        assert agent.appview_did == APPVIEW_DID
        assert "POST api.engram.test/xrpc/garden.engram.warmSpace" in world.calls
