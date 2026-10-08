"""Checks against fixtures the Go implementation made (tests/fixtures/go.json). The other direction,
Go verifying what Python made, is internal/lex/pyinterop_test.go."""

from __future__ import annotations

import json
import struct
from pathlib import Path

import pytest
from cryptography.hazmat.primitives.asymmetric import ec

from engram_garden import lex, space, vec

GO = json.loads((Path(__file__).parent / "fixtures" / "go.json").read_text())


def floats_of(bits: list[int]) -> list[float]:
    return [struct.unpack("<f", struct.pack("<I", b))[0] for b in bits]


def test_did_key_matches_go():
    key = ec.derive_private_key(int(GO["privateKeyHex"], 16), ec.SECP256R1())
    assert space.did_key(key.public_key()) == GO["didKey"]


def test_did_key_round_trip():
    pub = space.public_key_from_did_key(GO["didKey"])
    assert space.did_key(pub) == GO["didKey"]


@pytest.mark.parametrize("i", range(len(GO["sigs"])))
def test_verifies_go_signatures(i):
    s = GO["sigs"][i]
    signer = space.verify_space_signature(s["headers"], s["keyId"])
    assert signer == GO["didKey"]


def test_go_signature_rejects_tampering():
    s = GO["sigs"][1]
    headers = dict(s["headers"])
    headers["authorization"] = "Atproto-Space other.credential.jwt"
    with pytest.raises(ValueError):
        space.verify_space_signature(headers, s["keyId"])


def test_signature_shape_matches_go():
    """Same header names and structure, whatever the (random) signature bytes."""
    key = ec.derive_private_key(int(GO["privateKeyHex"], 16), ec.SECP256R1())
    for s in GO["sigs"]:
        mine = space.create_space_sig_headers(key, s["authorization"], s["audience"])
        assert set(mine) == set(s["headers"])
        for name in ("authorization", "signature-input", "atproto-space-audience"):
            assert mine.get(name) == s["headers"].get(name)
        assert len(mine["signature"]) == len(s["headers"]["signature"])


@pytest.mark.parametrize("i", range(len(GO["vectors"])))
def test_vectors_match_go(i):
    v = GO["vectors"][i]
    floats = floats_of(v["bits"])
    assert vec.encode_f16(floats).hex() == v["f16Hex"]
    assert lex.encode_query_vector(floats) == v["queryVector"]
    m = lex.ModelInfo("nomic-embed-text", "sha256:0a109f42", len(floats))
    assert lex.embedding_record(m, floats) == v["record"]
    # And what Go wrote reads back.
    (e,) = lex.parse_embeddings({"embedding": v["record"]})
    assert e.vector == vec.decode_f16(bytes.fromhex(v["f16Hex"]))
    assert lex.decode_query_vector(v["queryVector"]) == e.vector


@pytest.mark.parametrize("c", GO["embedText"])
def test_embed_text_matches_go(c):
    assert lex.embed_text(c["prefix"], c["text"], c["tags"]) == c["want"]
