"""Skills: saved in the agent's own repo, read from every member's, one version per name."""

from __future__ import annotations

import pytest

import engram_garden as eg
from engram_garden import lex

from .conftest import make_settings
from .fakes import AGENT_DID, AUTHORITY_DID, OTHER_DID, SPACE, World


def other_skill(
    world: World, name: str, instructions: str, by: str = OTHER_DID, updated: str = "2026-01-02T00:00:00.000Z"
) -> None:
    world.put(
        SPACE,
        by,
        lex.SKILL_COLLECTION,
        name,
        {
            "$type": lex.SKILL_COLLECTION,
            "name": name,
            "description": "d",
            "instructions": instructions,
            "createdAt": updated,
            "updatedAt": updated,
        },
    )


async def test_save_list_and_update(world: World, http):
    async with await eg.open_spaces(make_settings(), http=http) as s:
        out = await s.save_skill(
            "code-review",
            "Review a change. Use after finishing work.",
            "# Review\n\nAsk two models.",
            files=[eg.SkillFile("scripts/run.sh", "#!/bin/sh\n", executable=True)],
        )
        assert out.created and out.uri.endswith(f"/{AGENT_DID}/garden.engram.skill/code-review")
        listed = await s.list_skills()
        assert [sk.name for sk in listed.skills] == ["code-review"]
        assert listed.skills[0].skill.instructions == ""  # no content unless asked
        out = await s.save_skill("code-review", "Review a change.", "Ask three models.", note="two wasn't enough")
        assert not out.created
        got = await s.get_skill("code-review")
        assert "three models" in got.skill.instructions
        assert got.skill.files[0].executable  # kept when left out
        assert got.skill.note == "two wasn't enough"
        assert got.skill.created_at <= got.skill.updated_at
        assert got.to_dict()["space"] == "memory"


async def test_versions_resolve(world: World, http):
    other_skill(world, "deploy", "the other agent's way", updated="2026-03-01T00:00:00.000Z")
    other_skill(world, "deploy", "the authority's way", by=AUTHORITY_DID, updated="2026-01-01T00:00:00.000Z")
    async with await eg.open_spaces(make_settings(), http=http) as s:
        # Not having one of its own, the agent gets the authority's, though another's is newer.
        got = await s.get_skill("deploy")
        assert got.skill.instructions == "the authority's way"
        assert len(got.shadowed) == 1
        out = await s.save_skill("deploy", "d", "my way")
        assert "also keeps a skill named deploy" in out.note
        assert (await s.get_skill("deploy")).skill.instructions == "my way\n"


async def test_invalid_records_are_skipped(world: World, http):
    world.put(
        SPACE,
        OTHER_DID,
        lex.SKILL_COLLECTION,
        "wrong-key",
        {"name": "real", "description": "d", "instructions": "i", "createdAt": "x", "updatedAt": "x"},
    )
    world.put(SPACE, OTHER_DID, lex.SKILL_COLLECTION, "bare", {"name": "bare", "instructions": "i"})
    async with await eg.open_spaces(make_settings(), http=http) as s:
        assert (await s.list_skills()).skills == []


async def test_delete_only_your_own(world: World, http):
    other_skill(world, "theirs", "x")
    async with await eg.open_spaces(make_settings(), http=http) as s:
        with pytest.raises(eg.EngramError, match="only delete your own"):
            await s.delete_skill("theirs")
        await s.save_skill("mine", "d", "i")
        uri = await s.delete_skill("mine")
        assert uri.endswith("/garden.engram.skill/mine")
        assert [sk.name for sk in (await s.list_skills()).skills] == ["theirs"]


@pytest.mark.parametrize(
    "kw",
    [
        {"name": "Bad", "description": "d", "instructions": "i"},
        {"name": "ok", "description": " ", "instructions": "i"},
        {"name": "ok", "description": "d", "instructions": "i", "files": [eg.SkillFile("../x", "")]},
    ],
)
async def test_bad_input(world: World, http, kw):
    async with await eg.open_spaces(make_settings(), http=http) as s:
        with pytest.raises(eg.EngramError):
            await s.save_skill(**kw)


def test_validation_matches_go():
    for name, ok in {"code-review": True, "a--b": False, "-a": False, "a" * 64: True, "a" * 65: False}.items():
        assert eg.valid_skill_name(name) == ok


T = "2026-01-01T00:00:00.000Z"


def test_paths_a_directory_would_drop_are_refused():
    for p in ("scripts/.env", ".gitignore", "node_modules/x.js", "a/__pycache__/b"):
        with pytest.raises(eg.EngramError, match="aren't kept"):
            eg.Skill("x", "d", "i", files=[eg.SkillFile(p, "")], created_at=T, updated_at=T).validate()
    with pytest.raises(eg.EngramError, match="skill of its own"):
        eg.Skill("x", "d", "i", files=[eg.SkillFile("sub/SKILL.md", "")], created_at=T, updated_at=T).validate()
    with pytest.raises(eg.EngramError, match="isn't text"):
        eg.Skill("x", "d", "i", files=[eg.SkillFile("a", "x\x00y")], created_at=T, updated_at=T).validate()


async def test_unreadable_member_is_noted(world: World, http):
    other_skill(world, "theirs", "x")
    async with await eg.open_spaces(make_settings(), http=http) as s:
        await s.save_skill("mine", "d", "i")
        world.reject_member_reads = {OTHER_DID}
        out = await s.list_skills()
        assert [sk.name for sk in out.skills] == ["mine"]
        assert f"couldn't read {OTHER_DID}'s skills" in out.note
        saved = await s.save_skill("mine", "d", "i2")
        assert saved.space == "memory"


@pytest.mark.parametrize(
    "bad",
    [
        {"disableModelInvocation": "false"},
        {"files": [{"path": 1, "content": "x"}]},
        {"files": [{"path": "a", "content": "x", "executable": "yes"}]},
        {"description": 3},
        {"files": 0},
        {"updatedAt": "2026-01-02t15:04:05z"},
    ],
)
async def test_malformed_records_are_skipped_like_go(world: World, http, bad):
    rec = {"name": "x", "description": "d", "instructions": "i", "createdAt": "c", "updatedAt": "u", **bad}
    world.put(SPACE, OTHER_DID, lex.SKILL_COLLECTION, "x", rec)
    async with await eg.open_spaces(make_settings(), http=http) as s:
        assert (await s.list_skills()).skills == []


def test_resolve_by_instant_like_go():
    from engram_garden.skills import resolve

    def v(uri: str, updated: str) -> eg.SkillInfo:
        return eg.SkillInfo(eg.Skill("x", "d", "i", updated_at=updated), uri=uri, cid="", author="did:plc:" + uri)

    got = resolve([v("a", "2026-01-21T23:00:00+02:00"), v("b", "2026-01-21T22:00:00.000Z")], "did:plc:me", "x")
    assert got[0].uri == "b"
    got = resolve([v("a", "9999-01-01T00:00:00Z"), v("b", "2026-01-21T22:00:00.000Z")], "did:plc:me", "x")
    assert got[0].uri == "b"
    with pytest.raises(eg.EngramError, match="RFC 3339"):
        eg.Skill("x", "d", "i", created_at="Oct 21", updated_at="Oct 21").validate()
