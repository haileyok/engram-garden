// Client for the web app's backend (internal/web). Every call is
// same-origin and carries the session cookie.

export type Memory = {
  uri: string;
  cid: string;
  author: string;
  text: string;
  tags: string[];
  source?: string;
  createdAt: string;
  indexedAt: string;
};

export type ModelInfo = { model: string; modelDigest: string; dims: number };

export type SpaceConfig = ModelInfo & {
  documentPrefix?: string;
  queryPrefix?: string;
  next?: ModelInfo;
};

export type SpaceStatus = {
  config?: SpaceConfig;
  active?: ModelInfo;
  building?: ModelInfo;
  memories: number;
  buildingMemories?: number;
  skipped: { author: string; count: number }[];
  // Whether the appview may read the space to keep its index current.
  access?: Access;
};

export type Access = {
  state: "granted" | "missing" | "lapsed";
  grantedBy?: string;
  grantedAt?: string;
  error?: string;
};

export type SpaceSummary = {
  uri: string;
  authority: string;
  name: string;
  isAuthority: boolean;
};

export type Member = { did: string; read: boolean; write: boolean };

export type Session = { did: string; handle?: string; appviewDid: string };

export type Service = { did: string; grantUrl?: string; registration: "open" | "closed" };

export type ConfigAction = "declare" | "next" | "promote" | "cancel";

export type MemoryFilters = { author?: string; tags?: string[]; since?: string };

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message || code);
    this.status = status;
    this.code = code;
  }
}

async function call<T>(method: "GET" | "POST", path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: "same-origin", headers: {} };
  if (method === "POST") {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body ?? {});
  }
  let resp: Response;
  try {
    resp = await fetch(path, init);
  } catch {
    throw new ApiError(0, "NetworkError", "couldn't reach the server");
  }
  const text = await resp.text();
  let data: any = undefined;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    // Not JSON.
  }
  if (!resp.ok) {
    throw new ApiError(resp.status, data?.error ?? resp.statusText, data?.message ?? text);
  }
  return data as T;
}

function qs(params: Record<string, string | string[] | undefined>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === "") continue;
    for (const one of Array.isArray(v) ? v : [v]) p.append(k, one);
  }
  const s = p.toString();
  return s ? `?${s}` : "";
}

export const api = {
  session: () => call<Session>("GET", "/api/session"),
  login: (handle: string) => call<{ redirect: string }>("POST", "/api/login", { handle }),
  logout: () => call<{}>("POST", "/api/logout"),
  service: () => call<Service>("GET", "/api/service"),
  profiles: (dids: string[]) =>
    call<{ handles: Record<string, string> }>("GET", "/api/profiles" + qs({ did: dids })),

  spaces: () => call<{ spaces: SpaceSummary[] }>("GET", "/api/spaces"),
  status: (space: string) => call<SpaceStatus>("GET", "/api/status" + qs({ space })),
  memories: (space: string, f: MemoryFilters, cursor?: string, limit = 25) =>
    call<{ memories: Memory[]; cursor?: string }>(
      "GET",
      "/api/memories" +
        qs({ space, author: f.author, tags: f.tags, since: f.since, cursor, limit: String(limit) }),
    ),
  memory: (space: string, uri: string) =>
    call<{ memory: Memory }>("GET", "/api/memory" + qs({ space, uri })),
  deleteMemory: (space: string, uri: string) =>
    call<{ deleted: string }>("POST", "/api/memories/delete", { space, uri }),

  createSpace: (name: string) => call<{ uri: string }>("POST", "/api/spaces/create", { name }),
  members: (space: string) => call<{ members: Member[] }>("GET", "/api/members" + qs({ space })),
  putMember: (space: string, member: string, read: boolean, write: boolean) =>
    call<Member>("POST", "/api/members/put", { space, member, read, write }),
  removeMember: (space: string, did: string) =>
    call<{}>("POST", "/api/members/remove", { space, did }),
  config: (space: string) =>
    call<{ config: SpaceConfig | null }>("GET", "/api/config" + qs({ space })),
  putConfig: (
    space: string,
    action: ConfigAction,
    model?: ModelInfo,
    prefixes?: { documentPrefix?: string; queryPrefix?: string },
  ) =>
    call<{ config: SpaceConfig }>("POST", "/api/config", {
      space,
      action,
      model,
      documentPrefix: prefixes?.documentPrefix ?? "",
      queryPrefix: prefixes?.queryPrefix ?? "",
    }),
};
