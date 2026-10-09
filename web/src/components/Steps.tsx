import type { ReactNode } from "react";

// Numbered steps. A step's terminal sits beside its text. The numbers come from
// a CSS counter, so steps can be added or moved without renumbering.
export function Steps({ children }: { children: ReactNode }) {
  return <ol className="steps">{children}</ol>;
}

export function Step({ title, children, aside }: { title: string; children?: ReactNode; aside?: ReactNode }) {
  return (
    <li className="step">
      <div className="step-text">
        <h3>{title}</h3>
        {children && <p>{children}</p>}
      </div>
      {aside && <div className="step-aside">{aside}</div>}
    </li>
  );
}
