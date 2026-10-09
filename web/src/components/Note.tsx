import { useState, type CSSProperties } from "react";
import type { Memory } from "../api";
import { authorHue } from "../lib/colors";
import { Handle } from "../profiles";
import { formatTime, relativeTime } from "../ui";

// Note is one memory in a list: who wrote it and when, the text, its tags,
// and, when asked, the record's details. A memory from a search also shows
// how close it is to the question.
export function Note({
  memory: m,
  me,
  fresh,
  onAuthor,
  onTag,
  onDelete,
}: {
  memory: Memory;
  me: string;
  fresh?: boolean;
  onAuthor?: (did: string) => void;
  onTag?: (tag: string) => void;
  onDelete?: (m: Memory) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <li className={fresh ? "note fresh" : "note"} style={{ "--hue": authorHue(m.author) } as CSSProperties}>
      <div className="note-head">
        <button type="button" className="author" onClick={() => onAuthor?.(m.author)} title="Show only this author">
          <Handle did={m.author} />
        </button>
        <span className="note-time" title={formatTime(m.createdAt)}>
          {relativeTime(m.createdAt)}
        </span>
        {m.similarity !== undefined && (
          <span className="note-sim" title="How close this is to your search">
            {(m.similarity / 1000).toFixed(2)}
          </span>
        )}
      </div>
      <p className="note-text">{m.text}</p>
      <div className="note-foot">
        {m.tags.map((t) => (
          <button type="button" key={t} className="tag" onClick={() => onTag?.(t)}>
            #{t}
          </button>
        ))}
        {m.source && <span className="note-source">from {m.source}</span>}
        <span className="note-actions">
          <button type="button" className="link small" onClick={() => setOpen(!open)}>
            {open ? "hide details" : "details"}
          </button>
          {m.author === me && onDelete && (
            <button type="button" className="link small danger" onClick={() => onDelete(m)}>
              delete
            </button>
          )}
        </span>
      </div>
      {open && (
        <dl className="details small">
          <dt>URI</dt>
          <dd>
            <code>{m.uri}</code>
          </dd>
          <dt>CID</dt>
          <dd>
            <code>{m.cid}</code>
          </dd>
          <dt>Written</dt>
          <dd>{formatTime(m.createdAt)}</dd>
          <dt>Indexed</dt>
          <dd>{formatTime(m.indexedAt)}</dd>
        </dl>
      )}
    </li>
  );
}
