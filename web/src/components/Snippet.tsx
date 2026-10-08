import { CopyButton } from "./CopyButton";

// Snippet is a block of commands or config with a copy button.
export function Snippet({ text }: { text: string }) {
  return (
    <div className="snippet">
      <pre>
        <code>{text}</code>
      </pre>
      <CopyButton text={text} />
    </div>
  );
}
