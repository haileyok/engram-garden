import { describe, expect, it, vi } from "vitest";
import { grantLink, memoryAuthor, parseSpaceUri, spacePath, validSpaceName } from "./uri";
import { knownPrefixes, listLocalModels, normalizeDigest, normalizeModelName } from "./ollama";

const space = "at://did:plc:abc123/space/garden.engram.space/team";

describe("space URIs", () => {
  it("parses memory spaces", () => {
    expect(parseSpaceUri(space)).toEqual({
      authority: "did:plc:abc123",
      type: "garden.engram.space",
      name: "team",
      uri: space,
    });
    expect(parseSpaceUri("https://example.com")).toBeNull();
    expect(parseSpaceUri("at://did:plc:abc/space/garden.engram.space/")).toBeNull();
  });
  it("round-trips through the page path", () => {
    expect(spacePath(space)).toBe("/space/did%3Aplc%3Aabc123/team");
  });
  it("finds a memory's author", () => {
    expect(memoryAuthor(space, `${space}/did:plc:alice/garden.engram.memory/3k`)).toBe("did:plc:alice");
    expect(memoryAuthor(space, "at://did:plc:other/space/x/y/did:plc:a/c/r")).toBeNull();
  });
  it("links to the appview's grant page", () => {
    const u = new URL(grantLink("https://api.engram.test/oauth/grant", space, "stop", "https://engram.test/space/x?tab=manage"));
    expect(u.origin + u.pathname).toBe("https://api.engram.test/oauth/grant");
    expect(u.searchParams.get("space")).toBe(space);
    expect(u.searchParams.get("mode")).toBe("stop");
    expect(u.searchParams.get("return")).toBe("https://engram.test/space/x?tab=manage");
  });
  it("checks space names", () => {
    expect(validSpaceName("team-memory")).toBe(true);
    expect(validSpaceName("has space")).toBe(false);
    expect(validSpaceName("..")).toBe(false);
  });
});

describe("local models", () => {
  it("records names and digests the way engram-config does", () => {
    expect(normalizeModelName("nomic-embed-text:latest")).toBe("nomic-embed-text");
    expect(normalizeModelName("mxbai-embed-large:335m")).toBe("mxbai-embed-large:335m");
    expect(normalizeDigest("0a1b")).toBe("sha256:0a1b");
    expect(normalizeDigest("sha256:0a1b")).toBe("sha256:0a1b");
    expect(knownPrefixes("nomic-embed-text").queryPrefix).toBe("search_query: ");
    expect(knownPrefixes("mxbai-embed-large").queryPrefix).toBe("");
  });
  it("lists Ollama's models", async () => {
    const fetchMock = vi.fn(async () =>
      new Response(JSON.stringify({ models: [{ name: "nomic-embed-text:latest", digest: "abc" }, { name: "x" }] })),
    );
    vi.stubGlobal("fetch", fetchMock);
    expect(await listLocalModels("http://ollama.test")).toEqual([{ name: "nomic-embed-text", digest: "sha256:abc" }]);
    expect(fetchMock).toHaveBeenCalledWith("http://ollama.test/api/tags");
    vi.unstubAllGlobals();
  });
});
