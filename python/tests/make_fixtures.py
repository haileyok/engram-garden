"""Write tests/fixtures/python.json: what the Go tests (internal/lex/pyinterop_test.go) check.

Run from python/:  uv run python tests/make_fixtures.py
Then, from the repository root:  go test ./internal/lex/ -run TestPythonFixtures -v
"""

from __future__ import annotations

import json
import struct
from pathlib import Path

from engram_garden import lex, space, vec

FIXTURES = Path(__file__).parent / "fixtures"


def main() -> None:
    go = json.loads((FIXTURES / "go.json").read_text())
    key = space.generate_key()
    did_key = space.did_key(key.public_key())
    sigs = []
    for auth, aud in [
        ("Bearer eyJ.delegation.token", ""),
        ("Atproto-Space eyJ.credential.jwt", "did:web:api.engram.garden"),
        ("Atproto-Space eyJ.credential.jwt", "did:plc:abcdefghijklmnopqrstuvwx"),
    ]:
        sigs.append(
            {
                "authorization": auth,
                "audience": aud,
                "keyId": did_key if aud else "",
                "headers": space.create_space_sig_headers(key, auth, aud),
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
        json.dumps({"didKey": did_key, "sigs": sigs, "vectors": vectors}, indent=2) + "\n"
    )


if __name__ == "__main__":
    main()
