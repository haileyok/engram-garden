import { useEffect, useState, type FormEvent } from "react";
import type { Recall } from "../lib/useRecall";

// SearchBox searches the space by meaning. Its answers come back through the
// Recall it's given, so the list and the graph can both show them.
export function SearchBox({ recall, placeholder }: { recall: Recall; placeholder?: string }) {
  const { state, unavailable } = recall;
  const [text, setText] = useState("");
  // Clearing from elsewhere (switching space) empties the box too.
  useEffect(() => {
    if (state.phase === "idle") setText("");
  }, [state.phase]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    recall.run(text);
  };
  const busy = state.phase === "searching";
  const searched = state.phase === "done" || state.phase === "error";
  return (
    <div className="recall">
      <form onSubmit={submit} className="recall-box" role="search">
        <label htmlFor="recall-q" className="sr-only">
          Search this space by meaning
        </label>
        <input
          id="recall-q"
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={unavailable ?? placeholder ?? "Search by meaning, like “how do we deploy?”"}
          disabled={unavailable !== null}
          autoComplete="off"
          maxLength={4000}
        />
        {searched && (
          <button
            type="button"
            className="link"
            onClick={() => {
              setText("");
              recall.clear();
            }}
          >
            clear
          </button>
        )}
        <button className="primary" disabled={busy || unavailable !== null || !text.trim()}>
          {busy ? "Searching…" : "Search"}
        </button>
      </form>
      {state.phase === "error" && (
        <p className="recall-error" role="alert">
          {state.message}
        </p>
      )}
    </div>
  );
}
