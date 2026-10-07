import { ApiError } from "./api";

// Messages for errors people can act on.
export function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) {
      window.dispatchEvent(new Event("engram:signed-out"));
      return "Your sign-in expired. Sign in again.";
    }
    switch (e.code) {
      case "UserNotAuthorized":
      case "Forbidden":
        return "You're not a member of this space.";
      case "SpaceNotFound":
        return "That space doesn't exist.";
      case "SpaceDeleted":
        return "That space was deleted.";
      case "IndexLoading":
        return "The appview is loading this space. Try again in a moment.";
      case "NotOwner":
        return "The appview is busy moving this space between servers. Try again shortly.";
    }
    return e.message || e.code;
  }
  return e instanceof Error ? e.message : String(e);
}

export function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

export function relativeTime(iso: string, now = Date.now()): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const s = Math.round((now - t) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  if (s < 30 * 86400) return `${Math.floor(s / 86400)}d ago`;
  return formatTime(iso);
}
