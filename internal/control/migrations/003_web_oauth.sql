-- The web app's own OAuth sessions and sign-ins in progress, kept in tables
-- apart from the appview's (oauth_sessions, oauth_requests): the appview's
-- sweep deletes the sessions it lists that none of its grants use.

CREATE TABLE web_sessions (
    did        text NOT NULL,
    session_id text NOT NULL,
    data       jsonb NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (did, session_id)
);

-- Keyed by a hash of the OAuth state, like the appview's.
CREATE TABLE web_requests (
    state_hash text PRIMARY KEY,
    data       jsonb NOT NULL,
    created_at timestamptz NOT NULL
);
