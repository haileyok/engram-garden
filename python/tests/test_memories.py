"""remember, recall, get, list and forget, end to end against the fake network."""

from __future__ import annotations

import asyncio

import pytest

import engram_garden as eg
from engram_garden import lex, vec

from .conftest import make_settings
from .fakes import AGENT_DID, APPVIEW_URL, MODEL, OTHER_DID, SPACE, World


async def test_remember_writes_a_record_with_a_vector(spaces: eg.Spaces, world: World):
    out = await spaces.remember(
        "Deploys go through the deploy repo", tags=["ops", " infra "], source=" docs/deploy.md "
    )
    assert out.uri.startswith(f"{SPACE}/{AGENT_DID}/garden.engram.memory/")
    assert out.note == ""
    (rec,) = world.memories(SPACE, AGENT_DID).values()
    assert rec["text"] == "Deploys go through the deploy repo"
    assert rec["tags"] == ["ops", "infra"]
    assert rec["source"] == "docs/deploy.md"
    assert rec["createdAt"].endswith("Z")
    (emb,) = lex.parse_embeddings(rec)
    assert emb.model_info == MODEL
    assert vec.normalize(list(emb.vector))  # unit length
    assert abs(sum(x * x for x in emb.vector) - 1) < 1e-2


async def test_recall_ranks_by_similarity_across_agents(spaces: eg.Spaces, world: World):
    world.add_other_memory("the staging cluster lives in us-east and is cheap")
    world.add_other_memory("bananas are yellow fruit")
    await spaces.remember("production deploys need a second reviewer")
    out = await spaces.recall("how do production deploys work", limit=3)
    assert out.memories[0].text == "production deploys need a second reviewer"
    assert out.memories[0].space == "memory"
    assert out.memories[0].author == AGENT_DID
    assert out.memories[0].similarity is not None
    sims = [m.similarity for m in out.memories]
    assert sims == sorted(sims, reverse=True)
    assert out.note == ""


async def test_recall_filters(spaces: eg.Spaces, world: World):
    world.add_other_memory("alpha topic notes", tags=["a"])
    world.add_other_memory("alpha topic more notes", tags=["a", "b"])
    await spaces.remember("alpha topic mine", tags=["b"])
    both = await spaces.recall("alpha topic", tags=["a", "b"])
    assert [m.text for m in both.memories] == ["alpha topic more notes"]
    mine = await spaces.recall("alpha topic", author=AGENT_DID)
    assert [m.text for m in mine.memories] == ["alpha topic mine"]
    theirs = await spaces.recall("alpha topic", author=OTHER_DID)
    assert len(theirs.memories) == 2
    recent = await spaces.recall("alpha topic", since="2026-06-01T00:00:00.000Z")
    assert [m.text for m in recent.memories] == ["alpha topic mine"]


async def test_get_list_forget(spaces: eg.Spaces, world: World):
    a = await spaces.remember("first memory")
    await asyncio.sleep(0.002)
    b = await spaces.remember("second memory")
    got = await spaces.get(a.uri)
    assert got.memory.text == "first memory"
    assert got.memory.space == "memory"
    page = await spaces.list(limit=1)
    assert [m.text for m in page.memories] == ["second memory"]
    assert page.cursor
    nxt = await spaces.list(limit=1, cursor=page.cursor)
    assert [m.text for m in nxt.memories] == ["first memory"]
    out = await spaces.forget(b.uri)
    assert out.deleted == b.uri
    assert b.uri not in world.memories(SPACE)
    assert [m.text for m in (await spaces.list()).memories] == ["first memory"]


async def test_forget_only_your_own(spaces: eg.Spaces, world: World):
    theirs = world.add_other_memory("someone else's memory")
    with pytest.raises(eg.EngramError, match="belongs to"):
        await spaces.forget(theirs)
    with pytest.raises(eg.EngramError, match="not a record URI"):
        await spaces.forget("at://did:plc:x/not/a/uri")
    assert theirs in world.memories(SPACE)


async def test_remember_validates(spaces: eg.Spaces):
    with pytest.raises(eg.EngramError, match="text is required"):
        await spaces.remember("   ")
    with pytest.raises(eg.EngramError, match="too long"):
        await spaces.remember("x" * 30001)
    with pytest.raises(eg.EngramError, match="at most 16 tags"):
        await spaces.remember("ok", tags=[str(i) for i in range(17)])
    with pytest.raises(eg.EngramError, match="too long"):
        await spaces.remember("ok", tags=["t" * 129])
    with pytest.raises(eg.EngramError, match="query is required"):
        await spaces.recall(" ")


async def test_remember_keeps_a_given_created_at(spaces: eg.Spaces, world: World):
    out = await spaces.remember("an old memory", created_at="2026-02-22T10:00:00.000Z")
    assert world.memories(SPACE)[out.uri]["createdAt"] == "2026-02-22T10:00:00.000Z"
    from datetime import UTC, datetime

    out = await spaces.remember("another", created_at=datetime(2026, 3, 1, 12, 30, 15, 123456, tzinfo=UTC))
    assert world.memories(SPACE)[out.uri]["createdAt"] == "2026-03-01T12:30:15.123Z"


async def test_unicode_and_long_text_embed_text_is_bounded(spaces: eg.Spaces, world: World):
    text = "ünïcödé " * 2200  # over MAX_EMBED_CHARS in bytes, under the memory limit
    assert lex.MAX_EMBED_CHARS < len(text.encode()) < 30000
    out = await spaces.remember(text)
    assert world.memories(SPACE)[out.uri]["text"] == text.strip()


async def test_not_indexed_is_reported(spaces: eg.Spaces, world: World):
    world.access_state = "missing"
    out = await spaces.remember("stored but not searchable")
    assert out.note.startswith("Stored, but the appview can't read this space (missing")
    assert out.uri in world.memories(SPACE)
    empty = await spaces.recall("anything at all")
    assert "Nothing came back" in empty.note or empty.memories


async def test_recall_empty_space_says_why_when_not_indexed(spaces: eg.Spaces, world: World):
    world.access_state = "lapsed"
    out = await spaces.recall("nothing here", space="memory")
    assert out.memories == []
    assert "Nothing came back, and the appview can't read this space (lapsed" in out.note


async def test_model_mismatch_refreshes_the_config_once(spaces: eg.Spaces, world: World):
    await spaces.remember("a memory")
    agent, _ = spaces.agent()
    await agent.config()  # cache it
    other = lex.ModelInfo("hashing-32", MODEL.model_digest, 32)
    world.config = lex.Config(other).record()
    # The cached config is stale; the appview says ModelMismatch and the client rereads and retries.
    out = await spaces.recall("a memory")
    assert out.memories == []  # nothing is embedded with the new model yet
    assert (await agent.config()).model_info == other


async def test_no_model_declared(world: World, http):
    world.config = None
    async with await eg.open_spaces(make_settings(), http=http) as s:
        with pytest.raises(eg.EngramError, match="hasn't declared an embedding model"):
            await s.remember("hello")


async def test_local_model_mismatch_blocks_writes(world: World, http):
    world.config = lex.Config(lex.ModelInfo("hashing-64", "sha256:something-else", 64)).record()
    async with await eg.open_spaces(make_settings(), http=http) as s:
        with pytest.raises(eg.ModelMismatchError, match="not stored|digest"):
            await s.remember("hello")
        assert world.memories(SPACE) == {}


async def test_the_appview_is_called_with_a_credential_for_its_own_audience(spaces: eg.Spaces, world: World):
    await spaces.recall("anything")
    assert "GET api.engram.test/xrpc/garden.engram.searchMemories" in world.calls
    assert world.exchanges == 1
    await spaces.recall("anything again")
    assert world.exchanges == 1  # the credential is cached


async def test_a_rejected_credential_is_replaced_once(spaces: eg.Spaces, world: World):
    await spaces.recall("warm up")
    before = world.exchanges
    world.reject_credentials = 1
    await spaces.recall("retry")
    assert world.exchanges == before + 1


async def test_an_expired_session_is_refreshed(spaces: eg.Spaces, world: World):
    world.expire_tokens = 1
    out = await spaces.remember("written after the token expired")
    assert world.refreshes == 1
    assert out.uri in world.memories(SPACE)


async def test_a_dead_refresh_token_is_a_sign_in_error(spaces: eg.Spaces, world: World):
    world._refresh.clear()
    world.expire_tokens = 1
    with pytest.raises(eg.SignInExpiredError):
        await spaces.remember("this can't be written")


async def test_wrong_password(world: World, http):
    s = make_settings()
    s.account.password = "nope"
    with pytest.raises(eg.XrpcError) as e:
        await eg.open_spaces(s, http=http)
    assert e.value.status == 401


async def test_oauth_sign_in_is_explained(world: World, http):
    s = make_settings()
    s.account = eg.Account(
        did=AGENT_DID, sign_in=eg.SIGN_IN_OAUTH, session_id="x", callback="http://127.0.0.1:1/callback"
    )
    with pytest.raises(eg.SignInExpiredError, match="password sign-in"):
        await eg.open_spaces(s, http=http)


async def test_query_url_uses_the_appview(spaces: eg.Spaces):
    assert spaces.appview_url == APPVIEW_URL
