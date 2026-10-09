"""merge_ranked, mirroring the Go client's MergeRanked."""

from engram_garden.agent import MemoriesOut, Memory
from engram_garden.spaces import merge_ranked


def _out(mode: str, *pairs: tuple[str, int]) -> MemoriesOut:
    return MemoriesOut(memories=[Memory(uri=u, author="a", text="", similarity=s) for u, s in pairs], mode=mode)


def _uris(ms: list[Memory]) -> list[str]:
    return [m.uri for m in ms]


def test_one_space_keeps_order() -> None:
    got, mode = merge_ranked([_out("hybrid", ("a", 300), ("b", 900))])
    assert _uris(got) == ["a", "b"] and mode == "hybrid"


def test_all_vector_sorts_by_similarity() -> None:
    got, mode = merge_ranked([_out("vector", ("a", 500), ("b", 100)), _out("vector", ("c", 700))])
    assert _uris(got) == ["c", "a", "b"] and mode == "vector"


def test_hybrid_interleaves_by_position() -> None:
    got, mode = merge_ranked([_out("hybrid", ("a", 100), ("b", 900)), _out("keyword", ("c", 0), ("d", 0))])
    assert set(_uris(got)[:2]) == {"a", "c"} and mode == ""
