"""Engram Garden: shared memory for AI agents, on ATProto Spaces.

A Python client with the same operations as the Go `engram` tools:

    import engram_garden as eg

    settings = eg.load_settings()               # ~/.config/engram/config.json plus ENGRAM_* variables
    async with await eg.open_spaces(settings) as spaces:
        await spaces.remember("Deploys go through the deploy repo's workflow", tags=["ops"])
        found = await spaces.recall("how do we deploy")
"""

from .agent import Agent, ForgetOut, GetOut, MemoriesOut, Memory, RememberOut
from .embed import (
    HashingEmbedder,
    HashingProvider,
    OpenAIEmbedder,
    OpenAIProvider,
    ollama_digest,
    probe_dims,
)
from .errors import (
    EngramError,
    ModelMismatchError,
    SettingsError,
    SignInExpiredError,
    XrpcError,
    is_error,
)
from .identity import Directory, Identity
from .lex import (
    CONFIG_COLLECTION,
    MEMORY_COLLECTION,
    SPACE_TYPE,
    Config,
    ModelInfo,
)
from .manage import (
    CreateSpaceOut,
    IndexOut,
    Member,
    MemberOut,
    MembersOut,
    ModelOut,
    describe_model,
)
from .session import PdsSession
from .settings import (
    ALL_SPACES,
    DEFAULT_APPVIEW_URL,
    SIGN_IN_OAUTH,
    SIGN_IN_PASSWORD,
    Account,
    Embed,
    Settings,
    SpaceEntry,
    config_dir,
    config_path,
    load_settings,
)
from .space import Ref
from .spaceclient import Client
from .spaces import ListSpacesOut, SpaceInfo, Spaces, open_spaces, spaces_of

__version__ = "0.1.0"
