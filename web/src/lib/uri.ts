// Memory space URIs look like at://<authority DID>/space/garden.engram.space/<name>.

export const SPACE_TYPE = "garden.engram.space";

export type SpaceRef = { authority: string; type: string; name: string; uri: string };

export function parseSpaceUri(raw: string): SpaceRef | null {
  const m = /^at:\/\/(did:[a-z]+:[a-zA-Z0-9._:%-]+)\/space\/([a-zA-Z0-9.-]+)\/([a-zA-Z0-9._~:-]{1,512})$/.exec(
    raw.trim(),
  );
  if (!m) return null;
  const [, authority, type, name] = m;
  return { authority, type, name, uri: `at://${authority}/space/${type}/${name}` };
}

export function spaceUri(authority: string, name: string): string {
  return `at://${authority}/space/${SPACE_TYPE}/${name}`;
}

// The app's page for a space.
export function spacePath(uri: string): string {
  const ref = parseSpaceUri(uri);
  if (!ref) return "/";
  return `/space/${encodeURIComponent(ref.authority)}/${encodeURIComponent(ref.name)}`;
}

// The author of a memory, from its URI: <space>/<author>/<collection>/<rkey>.
export function memoryAuthor(spaceUri: string, memoryUri: string): string | null {
  if (!memoryUri.startsWith(spaceUri + "/")) return null;
  const parts = memoryUri.slice(spaceUri.length + 1).split("/");
  return parts.length === 3 ? parts[0] : null;
}

export function shortDid(did: string): string {
  if (did.length <= 24) return did;
  return did.slice(0, 14) + "…" + did.slice(-6);
}

// Space names are record keys.
export function validSpaceName(name: string): boolean {
  return /^[a-zA-Z0-9._~:-]{1,512}$/.test(name) && name !== "." && name !== "..";
}
