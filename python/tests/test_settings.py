"""The settings file (shared with the Go tools) and ENGRAM_* variables."""

from __future__ import annotations

import json
import os
import stat
from datetime import UTC, datetime

import pytest

import engram_garden as eg

URI_A = "at://did:plc:oisofpd7lj26yvgiivf3lxsi/space/garden.engram.space/coding-agents"
URI_B = "at://did:plc:oisofpd7lj26yvgiivf3lxsi/space/garden.engram.space/notes"

# What `engram login` writes (OAuth sign-in, fractional seconds with nine digits).
GO_FILE = {
    "spaces": [{"name": "coding-agents", "uri": URI_A}],
    "defaultSpace": "coding-agents",
    "appviewUrl": "https://api.engram.garden",
    "appviewDid": "did:web:api.engram.garden",
    "account": {
        "handle": "hailey.at",
        "did": "did:plc:oisofpd7lj26yvgiivf3lxsi",
        "signIn": "oauth",
        "sessionId": "G30DpJc6rGVWF3qklz-oaQ",
        "callback": "http://127.0.0.1:45777/callback",
        "signedInAt": "2026-10-08T07:22:13.888641669Z",
    },
    "somethingNew": {"kept": True},
}


def write(tmp_path, d) -> str:
    p = tmp_path / "config.json"
    p.write_text(json.dumps(d))
    return str(p)


def test_loads_the_file_the_go_tools_write(tmp_path):
    s = eg.load_settings(write(tmp_path, GO_FILE), getenv=None)
    assert s.spaces == [eg.SpaceEntry("coding-agents", URI_A)]
    assert s.default_space == "coding-agents"
    assert s.account.sign_in == eg.SIGN_IN_OAUTH
    assert s.account.signed_in_at == datetime(2026, 10, 8, 7, 22, 13, 888641, tzinfo=UTC)
    assert s.account.expires() == datetime(2026, 10, 22, 7, 22, 13, 888641, tzinfo=UTC)
    s.check()


def test_save_round_trips_and_keeps_unknown_keys(tmp_path):
    path = write(tmp_path, GO_FILE)
    s = eg.load_settings(path, getenv=None)
    s.add_space(URI_B)
    s.save(path)
    raw = json.loads((tmp_path / "config.json").read_text())
    assert raw["somethingNew"] == {"kept": True}
    assert raw["account"]["sessionId"] == "G30DpJc6rGVWF3qklz-oaQ"
    assert raw["account"]["signedInAt"].startswith("2026-10-08T07:22:13.888641")
    assert [e["name"] for e in raw["spaces"]] == ["coding-agents", "notes"]
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600
    again = eg.load_settings(path, getenv=None)
    assert again.account == s.account


def test_missing_file_is_empty_with_defaults(tmp_path):
    s = eg.load_settings(tmp_path / "nope.json", getenv=None)
    assert s.spaces == [] and s.appview_url == eg.DEFAULT_APPVIEW_URL
    assert s.appview_did == "did:web:api.engram.garden"
    with pytest.raises(eg.SettingsError, match="no memory space"):
        s.check()


def test_env_overrides(tmp_path):
    env = {
        "ENGRAM_SPACES": f"main={URI_A}, {URI_B}",
        "ENGRAM_SPACE": "notes",
        "ENGRAM_IDENTIFIER": "penny.hailey.at",
        "ENGRAM_PASSWORD": "pw",
        "ENGRAM_PDS_HOST": "https://cocoon.hailey.at",
        "ENGRAM_EMBED_URL": "http://ollama:11434/v1",
        "ENGRAM_EMBED_MODEL": "nomic-embed-text",
        "ENGRAM_APPVIEW_URL": "https://appview.example/",
    }
    s = eg.load_settings(write(tmp_path, GO_FILE), getenv=env.get)
    assert [e.name for e in s.spaces] == ["main", "notes"]
    assert s.default_space == "notes"
    assert s.account == eg.Account(
        handle="penny.hailey.at", sign_in="password", password="pw", pds_host="https://cocoon.hailey.at"
    )
    assert s.embed.url == "http://ollama:11434/v1" and s.embed.model == "nomic-embed-text"
    assert s.appview_url == "https://appview.example"
    assert s.appview_did == "did:web:appview.example"  # a new appview has its own DID
    s.check()


def test_env_errors_name_the_variable():
    with pytest.raises(eg.SettingsError, match="ENGRAM_SPACES"):
        eg.load_settings("/nonexistent", getenv={"ENGRAM_SPACES": "nonsense"}.get)
    with pytest.raises(eg.SettingsError, match="ENGRAM_SPACE"):
        eg.load_settings("/nonexistent", getenv={"ENGRAM_SPACE": "nobody"}.get)


def test_legacy_single_space_file(tmp_path):
    s = eg.load_settings(write(tmp_path, {"space": URI_A, "account": {}}), getenv=None)
    assert s.spaces == [eg.SpaceEntry("coding-agents", URI_A)] and s.default_space == "coding-agents"


def test_space_names():
    s = eg.Settings()
    assert s.add_space(URI_A).name == "coding-agents"
    assert s.add_space(URI_A).name == "coding-agents"  # already added keeps its entry
    other = "at://did:plc:xyz/space/garden.engram.space/coding-agents"
    assert s.add_space(other).name == "coding-agents-2"
    with pytest.raises(eg.SettingsError, match="already added"):
        s.add_space(URI_A, "renamed")
    with pytest.raises(eg.SettingsError, match="already used"):
        s.add_space(URI_B, "coding-agents")
    with pytest.raises(eg.SettingsError, match="reserved"):
        s.add_space(URI_B, "all")
    with pytest.raises(eg.SettingsError, match="can't contain"):
        s.add_space(URI_B, "a/b")
    with pytest.raises(eg.SettingsError, match="space"):
        s.add_space("not a uri")


def test_use_resolve_remove():
    s = eg.Settings()
    s.add_space(URI_A)
    s.add_space(URI_B)
    assert s.resolve("").name == "coding-agents"
    assert s.use_space("notes").name == "notes" and s.resolve("").name == "notes"
    assert s.resolve(URI_A).name == "coding-agents"
    one_off = "at://did:plc:xyz/space/garden.engram.space/elsewhere"
    assert s.resolve(one_off) == eg.SpaceEntry("elsewhere", one_off)
    assert [e.name for e in s.spaces] == ["coding-agents", "notes"]  # not added
    with pytest.raises(eg.SettingsError, match="no space named"):
        s.resolve("nobody")
    with pytest.raises(eg.SettingsError, match="use its at:// URI"):
        s.use_space("nobody")
    s.remove_space("notes")
    assert s.default_space == "coding-agents"
    s.remove_space("coding-agents")
    assert s.default_space == "" and s.default() is None
    with pytest.raises(eg.SettingsError, match="no memory space set up"):
        s.resolve("")


def test_check_account():
    s = eg.Settings()
    s.apply_defaults()
    with pytest.raises(eg.SettingsError, match="not signed in"):
        s.check_account()
    s.account = eg.Account(handle="h", sign_in="password")
    with pytest.raises(eg.SettingsError, match="handle and a password"):
        s.check_account()
    s.account.password = "x"
    s.check_account()
    s.appview_url = "nonsense"
    with pytest.raises(eg.SettingsError, match="isn't a URL"):
        s.check_account()
