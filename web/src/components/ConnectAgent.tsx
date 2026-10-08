import { useState } from "react";
import type { SpaceStatus } from "../api";
import { agentSetup, originOf } from "../lib/connect";
import { useService } from "./Indexing";

function Snippet({ text }: { text: string }) {
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

// ConnectAgent shows how an agent starts storing and recalling memories in
// this space with the engram CLI or engram-mcp.
export function ConnectAgent({ uri, status, isAuthority, onManage }: { uri: string; status: SpaceStatus | null; isAuthority: boolean; onManage: () => void }) {
  const service = useService();
  const setup = agentSetup(uri, originOf(service?.grantUrl));
  const model = status?.config;
  return (
    <div className="stack">
      <section className="card">
        <h2>Connect an agent</h2>
        <ol className="connect">
          <li>
            <strong>Give the agent its own account</strong> (on any PDS), and add it to this space as a member who can write
            {isAuthority ? (
              <>
                {" "}
                under <button type="button" className="link" onClick={onManage}>Manage</button>.
              </>
            ) : (
              <>. Only the space's authority can add members.</>
            )}
          </li>
          <li>
            <strong>Install</strong> the <code>engram</code> CLI and <code>engram-mcp</code>:
            <Snippet text={setup.install} />
          </li>
          <li>
            <strong>Set it up</strong> on the agent's machine. It signs in through your browser, checks the account can
            read this space, and checks the embedding model:
            <Snippet text={setup.init} />
            <p className="muted small">
              Browser sign-ins last two weeks (<code>engram login</code> renews them). On a machine without a browser, sign in
              with the account's password instead:
            </p>
            <Snippet text={setup.initHeadless} />
          </li>
          <li>
            <strong>Use it</strong> from a shell (add <code>--json</code> for machine-readable output):
            <Snippet text={setup.use} />
            or from an MCP client, which gets <code>remember</code> and <code>recall</code> tools:
            <Snippet text={setup.mcp} />
          </li>
        </ol>
        {model ? (
          <p className="muted small">
            Agents embed memories themselves with this space's model, <strong>{model.model}</strong>, through Ollama on their
            machine (<code>engram init</code> offers to pull it).
          </p>
        ) : (
          <p className="muted small">This space hasn't declared an embedding model yet; agents can't store memories until it does.</p>
        )}
      </section>
    </div>
  );
}
