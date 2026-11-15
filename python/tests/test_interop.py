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
    assert s["keyId"] == GO["didKey"]
    space.verify_credential_sig(s["headers"], s["keyId"])


def test_go_signature_rejects_tampering():
    s = GO["sigs"][0]
    headers = dict(s["headers"])
    headers["authorization"] = f"{space.CREDENTIAL_SCHEME} other.credential.jwt"
    with pytest.raises(ValueError):
        space.verify_credential_sig(headers, s["keyId"])


def test_go_signature_rejects_missing_tag():
    s = GO["sigs"][0]
    headers = dict(s["headers"])
    headers["signature-input"] = f'{space.SIG_LABEL}=("authorization" "atproto-space-audience")'
    with pytest.raises(ValueError):
        space.verify_credential_sig(headers, s["keyId"])


def test_go_signature_is_found_by_its_tag_under_any_label():
    s = GO["sigs"][0]
    headers = {k: v.replace("sig=", "other=", 1) if k.startswith("signature") else v for k, v in s["headers"].items()}
    space.verify_credential_sig(headers, s["keyId"])


def test_signature_shape_matches_go():
    """Same header names and structure, whatever the (random) signature bytes."""
    key = ec.derive_private_key(int(GO["privateKeyHex"], 16), ec.SECP256R1())
    for s in GO["sigs"]:
        mine = space.create_space_sig_headers(key, s["authorization"], s["audience"])
        assert set(mine) == set(s["headers"])
        for name in ("authorization", "signature-input", "atproto-space-audience"):
            assert mine.get(name) == s["headers"].get(name)
        assert len(mine["signature"]) == len(s["headers"]["signature"])


def _kem_of(m: dict) -> ec.EllipticCurvePrivateKey:
    return ec.derive_private_key(int(m["kemKeyHex"], 16), ec.SECP256R1())


@pytest.mark.parametrize("i", range(len(GO["macs"])))
def test_mac_secret_matches_go(i):
    m = GO["macs"][i]
    assert space.did_key(_kem_of(m).public_key()) == m["kemDidKey"]
    assert space.decapsulate(_kem_of(m), m["keyShare"]).hex() == m["sharedSecretHex"]


@pytest.mark.parametrize("i", range(len(GO["macs"])))
def test_mac_request_and_response_match_go(i):
    m = GO["macs"][i]
    assert space.format_mac_request(space.RECORD_MAC_PROFILE, m["keyShare"]) == m["acceptSignature"]
    req = space.parse_mac_request([m["acceptSignature"]], space.RECORD_MAC_PROFILE)
    assert req is not None and req.key_share == m["keyShare"]
    secret = bytes.fromhex(m["sharedSecretHex"])
    assert space.create_record_mac_headers(m["uri"], m["cid"], req, secret) == m["headers"]


@pytest.mark.parametrize("i", range(len(GO["macs"])))
def test_verifies_go_mac(i):
    m = GO["macs"][i]
    enc = space.Encapsulation(m["keyShare"], bytes.fromhex(m["sharedSecretHex"]))
    h = m["headers"]

    def verify(uri=m["uri"], cid=m["cid"], enc=enc, headers=h):
        space.verify_record_mac(
            uri, cid, enc, [headers["signature-input"]], [headers["signature"]], [headers["atproto-space-record"]]
        )

    verify()
    for bad in (
        lambda: verify(uri=m["uri"] + "x"),
        lambda: verify(cid=GO["macs"][0]["cid"][:-1] + "a"),
        lambda: verify(enc=space.Encapsulation(m["keyShare"], bytes(32))),
        lambda: verify(headers={**h, "atproto-space-record": h["atproto-space-record"].replace("uri=", "uri =")}),
    ):
        with pytest.raises(ValueError):
            bad()


def test_hpke_export_matches_the_reference_vector():
    """cocoon's space/kem_test.go: the export-only DHKEM(P-256, HKDF-SHA256) vector, with info
    "atproto-mac" and an empty exporter context."""
    enc = bytes.fromhex(
        "04d804370b7e24b94749eb1dc8df6d4d4a5d75f9effad01739ebcad5c54a40d57aaa8b4190fc124dbde2e4f1e1d1b012a3bc4038157dc29b55533a932306d8d38d"
    )
    sk = ec.derive_private_key(
        int("62c3868357a464f8461d03aa0182c7cebcde841036aea7230ddc7339f1088346", 16), ec.SECP256R1()
    )
    pub = ec.EllipticCurvePublicKey.from_encoded_point(ec.SECP256R1(), enc)
    got = space.decapsulate(sk, space.did_key(pub))
    assert got.hex() == "2060e0558d1b34c10086bc137f849bb79ba59fcdc30544418e35c9cd301f0d0d"


def test_encapsulation_agrees_with_decapsulation():
    kem = ec.generate_private_key(ec.SECP256R1())
    enc = space.encapsulate(space.did_key(kem.public_key()))
    assert space.decapsulate(kem, enc.key_share) == enc.shared_secret
    assert space.encapsulate(space.did_key(kem.public_key())).key_share != enc.key_share


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
