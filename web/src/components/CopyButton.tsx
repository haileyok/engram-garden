import { useState } from "react";

// CopyButton puts text on the clipboard and says so for a moment.
export function CopyButton({ text, className = "link small" }: { text: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard?.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };
  return (
    <button type="button" className={className} onClick={copy}>
      {copied ? "copied" : "copy"}
    </button>
  );
}
