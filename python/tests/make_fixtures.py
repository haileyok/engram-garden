"""Write tests/fixtures/python.json: what the Go tests (internal/lex/pyinterop_test.go) check.

Run from python/:  uv run python tests/make_fixtures.py
Then, from the repository root:  go test ./internal/lex/ -run TestPythonFixtures -v
"""

from __future__ import annotations

import json
import struct
from pathlib import Path

from cryptography.hazmat.primitives.asymmetric import ec

from engram_garden import lex, space, vec

FIXTURES = Path(__file__).parent / "fixtures"


def main() -> None:
    go = json.loads((FIXTURES / "go.json").read_text())
    key = space.generate_key()
    did_key = space.did_key(key.public_key())
    sigs = []
    for aud in ["did:web:api.engram.garden", "did:plc:abcdefghijklmnopqrstuvwx"]:
        auth = f"{space.CREDENTIAL_SCHEME} eyJ.credential.jwt"
        sigs.append(
            {
                "authorization": auth,
                "audience": aud,
                "keyId": did_key,
                "headers": space.create_space_sig_headers(key, auth, aud),
            }
        )
    # MACs: a repo host's answer to a record request, for Go to verify.
    kem_hex = go["macs"][0]["kemKeyHex"]
    kem = ec.derive_private_key(int(kem_hex, 16), ec.SECP256R1())
    macs = []
    for _ in range(2):
        enc = space.encapsulate(space.did_key(kem.public_key()))
        accept = space.format_mac_request(space.RECORD_MAC_PROFILE, enc.key_share)
        req = space.parse_mac_request([accept], space.RECORD_MAC_PROFILE)
        uri, cid = go["macs"][0]["uri"], go["macs"][0]["cid"]
        macs.append(
            {
                "kemKeyHex": kem_hex,
                "kemDidKey": space.did_key(kem.public_key()),
                "keyShare": enc.key_share,
                "sharedSecretHex": enc.shared_secret.hex(),
                "acceptSignature": accept,
                "uri": uri,
                "cid": cid,
                "headers": space.create_record_mac_headers(uri, cid, req, enc.shared_secret),
            }
        )
    vectors = []
    for v in go["vectors"]:
        floats = [struct.unpack("<f", struct.pack("<I", b))[0] for b in v["bits"]]
        m = lex.ModelInfo("nomic-embed-text", "sha256:0a109f42", len(floats))
        vectors.append(
            {
                "bits": v["bits"],
                "f16Hex": vec.encode_f16(floats).hex(),
                "queryVector": lex.encode_query_vector(floats),
                "record": lex.embedding_record(m, floats),
            }
        )
    (FIXTURES / "python.json").write_text(
        json.dumps({"didKey": did_key, "sigs": sigs, "macs": macs, "vectors": vectors}, indent=2) + "\n"
    )


if __name__ == "__main__":
    main()
