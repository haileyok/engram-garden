-- The appview's control-plane state. See internal/control.

-- Which authority let the appview read which space, and the OAuth session
-- that grant uses.
CREATE TABLE grants (
    space      text PRIMARY KEY,
    did        text NOT NULL,
    session_id text NOT NULL,
    granted_at timestamptz NOT NULL
);

-- OAuth sessions (indigo's session record: tokens, DPoP key, authorization
-- server URLs). Rewritten on every token refresh.
CREATE TABLE oauth_sessions (
    did        text NOT NULL,
    session_id text NOT NULL,
    data       jsonb NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (did, session_id)
);

-- OAuth sign-ins in progress. Keyed by a hash of the OAuth state, so the
-- table alone doesn't let anyone finish someone else's sign-in.
CREATE TABLE oauth_requests (
    state_hash text PRIMARY KEY,
    data       jsonb NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE oauth_pending (
    state_hash text PRIMARY KEY,
    space      text NOT NULL,
    mode       text NOT NULL,
    return_url text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);

-- Spaces the appview indexes because their authority granted access.
CREATE TABLE registered_spaces (
    space         text PRIMARY KEY,
    registered_at timestamptz NOT NULL
);
