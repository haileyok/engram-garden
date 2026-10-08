import { useState } from "react";

// Snippet is a block of commands or config with a copy button.
export function Snippet({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard?.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };
  return (
    <div className="snippet">
      <pre>
        <code>{text}</code>
      </pre>
      <button type="button" className="link small" onClick={copy}>
        {copied ? "copied" : "copy"}
      </button>
    </div>
  );
}
