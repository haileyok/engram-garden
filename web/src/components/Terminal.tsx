import type { ReactNode } from "react";
import { CopyButton } from "./CopyButton";

// A prompt line's start: the directory, then the prompt symbol.
export function Prompt({ cwd }: { cwd: string }) {
  return (
    <>
      <span className="t-cwd">{cwd}</span> <span className="t-sym">❯</span>{" "}
    </>
  );
}

// TermFrame is a terminal window, drawn after Ghostty's: a title bar with the
// window buttons, then the text. `copy` adds a copy button for the commands in
// it, so what's copied is the commands and not the prompts or output.
export function TermFrame({
  title,
  copy,
  className = "",
  children,
}: {
  title: string;
  copy?: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <figure className={`term ${className}`.trim()}>
      <div className="term-bar">
        <span className="term-dots" aria-hidden>
          <i />
          <i />
          <i />
        </span>
        <span className="term-title">{title}</span>
        <span className="term-end">{copy && <CopyButton text={copy} className="term-copy" />}</span>
      </div>
      <pre className="term-body">
        <code>{children}</code>
      </pre>
    </figure>
  );
}
