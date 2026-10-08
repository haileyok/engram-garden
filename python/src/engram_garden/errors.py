"""Errors the library raises."""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .lex import ModelInfo


class EngramError(Exception):
    """Base class for every error the library raises on purpose."""


class XrpcError(EngramError):
    """An XRPC error response (Go: spaceclient.Error)."""

    def __init__(self, status: int, name: str, message: str = "") -> None:
        self.status = status
        self.name = name
        self.message = message
        super().__init__(f"{status} {name}: {message}" if message else f"{status} {name}")


def is_error(err: BaseException, *names: str) -> bool:
    """Whether err is an XRPC error with one of the given names."""
    return isinstance(err, XrpcError) and err.name in names


class SignInExpiredError(EngramError):
    """The account's server no longer accepts the agent's sign-in."""

    def __init__(self, detail: str = "") -> None:
        msg = "the agent's sign-in has expired or was revoked: sign in again"
        super().__init__(f"{msg} ({detail})" if detail else msg)


class ModelMismatchError(EngramError):
    """The local embedding model isn't the one the space declares."""

    def __init__(self, want: ModelInfo, local_name: str, local: str) -> None:
        self.want = want
        self.local_name = local_name
        self.local = local
        if not local:
            msg = (
                f"the space uses {want}, but its digest couldn't be checked locally: pull the model "
                "into Ollama or set ENGRAM_EMBED_MODEL_DIGEST"
            )
        else:
            msg = (
                f"the space uses {want}, but the local {local_name} has digest {local}; pull the "
                f"declared model (for Ollama: ollama pull {want.model})"
            )
        super().__init__(msg)


class SettingsError(EngramError):
    """Missing or invalid settings."""


def explain(err: BaseException) -> BaseException:
    """Turn a refused sign-in into SignInExpiredError, keeping the original text."""
    if isinstance(err, SignInExpiredError):
        return err
    msg = str(err)
    if "token refresh failed" in msg or "session: not found" in msg:
        return SignInExpiredError(msg)
    return err
