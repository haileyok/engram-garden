// What an agent runs to use a memory space, for the "Connect an agent" panel.

export const DEFAULT_APPVIEW = "https://api.engram.garden";

export type AgentSetup = {
  install: string;
  init: string;
  initHeadless: string;
  // add is for an agent already set up with another space.
  add: string;
  use: string;
  mcp: string;
};

// appviewOrigin is the appview's origin (from its grant URL), when known.
export function agentSetup(space: string, appviewOrigin?: string): AgentSetup {
  const appview = appviewOrigin && appviewOrigin !== DEFAULT_APPVIEW ? ` --appview ${appviewOrigin}` : "";
  const init = `engram init --space ${space}${appview}`;
  return {
    install: "nix profile install github:haileyok/engram-garden",
    init,
    initHeadless: `${init} --password`,
    add: `engram spaces add ${space}`,
    use: 'engram remember "what to remember" -t tag\nengram recall "what to look for"',
    mcp: JSON.stringify({ mcpServers: { engram: { command: "engram-mcp" } } }, null, 2),
  };
}

// originOf returns a URL's origin, or undefined.
export function originOf(url?: string): string | undefined {
  if (!url) return undefined;
  try {
    return new URL(url).origin;
  } catch {
    return undefined;
  }
}
