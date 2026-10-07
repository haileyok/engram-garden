// Handles for DIDs, fetched in batches and shared across the app.

import { useEffect, useState } from "react";
import { api } from "./api";
import { shortDid } from "./lib/uri";

const handles = new Map<string, string | null>(); // null: no handle
const listeners = new Set<() => void>();
let pending = new Set<string>();
let timer: ReturnType<typeof setTimeout> | undefined;

function flush() {
  timer = undefined;
  const dids = [...pending];
  pending = new Set();
  for (let i = 0; i < dids.length; i += 100) {
    const batch = dids.slice(i, i + 100);
    api
      .profiles(batch)
      .then((r) => {
        for (const d of batch) handles.set(d, r.handles[d] ?? null);
      })
      .catch(() => {
        for (const d of batch) handles.set(d, null);
      })
      .finally(() => listeners.forEach((l) => l()));
  }
}

function request(did: string) {
  if (handles.has(did) || pending.has(did)) return;
  pending.add(did);
  timer ??= setTimeout(flush, 30);
}

export function useHandle(did: string): string | null | undefined {
  const [, bump] = useState(0);
  useEffect(() => {
    const l = () => bump((n) => n + 1);
    listeners.add(l);
    request(did);
    return () => {
      listeners.delete(l);
    };
  }, [did]);
  return handles.get(did);
}

export function Handle({ did }: { did: string }) {
  const h = useHandle(did);
  return (
    <span className="handle" title={did}>
      {h ? `@${h}` : shortDid(did)}
    </span>
  );
}
