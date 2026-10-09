-- Apps that connect to the web app over MCP (claude.ai), the accounts'
-- approvals of them, and approvals waiting to be traded for tokens.

CREATE TABLE mcp_clients (
    id            text PRIMARY KEY,
    name          text NOT NULL,
    redirect_uris text[] NOT NULL,
    created_at    timestamptz NOT NULL
);

-- Refresh tokens are kept as hashes. prev_refresh_hash is the one before,
-- so a token traded twice is noticed.
CREATE TABLE mcp_grants (
    id                text PRIMARY KEY,
    did               text NOT NULL,
    session_id        text NOT NULL,
    client_id         text NOT NULL,
    refresh_hash      text NOT NULL UNIQUE,
    prev_refresh_hash text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL,
    last_used_at      timestamptz NOT NULL
);

CREATE INDEX mcp_grants_did ON mcp_grants (did, created_at DESC);
CREATE INDEX mcp_grants_client ON mcp_grants (client_id);
CREATE INDEX mcp_grants_prev ON mcp_grants (prev_refresh_hash) WHERE prev_refresh_hash <> '';

-- Keyed by a hash of the code, like sign-ins in progress.
CREATE TABLE mcp_codes (
    code_hash    text PRIMARY KEY,
    client_id    text NOT NULL,
    redirect_uri text NOT NULL,
    challenge    text NOT NULL,
    did          text NOT NULL,
    session_id   text NOT NULL,
    expires_at   timestamptz NOT NULL
);
