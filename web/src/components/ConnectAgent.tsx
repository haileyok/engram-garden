import type { ReactNode } from "react";
import type { SpaceStatus } from "../api";
import { agentSetup, originOf } from "../lib/connect";
import { useService } from "./Indexing";
import { Step, Steps } from "./Steps";
import { Json, Prompt, TermFrame } from "./Terminal";

function Cmd({ children }: { children: ReactNode }) {
  return (
    <div className="t-line">
      <Prompt cwd="~" />
      {children}
    </div>
  );
}

function Comment({ children }: { children: ReactNode }) {
  return <div className="t-line t-dim"># {children}</div>;
}

// ConnectAgent shows how an agent starts storing and recalling memories in
// this space with the engram CLI or engram-mcp.
export function ConnectAgent({
  uri,
  status,
  isAuthority,
  onManage,
}: {
  uri: string;
  status: SpaceStatus | null;
  isAuthority: boolean;
  onManage: () => void;
}) {
  const service = useService();
  const setup = agentSetup(uri, originOf(service?.grantUrl));
  const model = status?.config;
  return (
    <section className="connect">
      <Steps>
        <Step title="Give the agent its own account">
          On any PDS. Add it to this space as a member who can write
          {isAuthority ? (
            <>
              {" "}
              under{" "}
              <button type="button" className="link" onClick={onManage}>
                Manage
              </button>
              .
            </>
          ) : (
            <>. Only the space's authority can add members.</>
          )}
        </Step>
        <Step
          title="Install the tools"
          aside={
            <TermFrame title="agent host" copy={setup.install}>
              <Cmd>{setup.install}</Cmd>
            </TermFrame>
          }
        >
          The <code>engram</code> CLI and <code>engram-mcp</code>.
        </Step>
        <Step
          title="Sign in on the agent's machine"
          aside={
            <TermFrame title="agent host" copy={setup.init}>
              <Cmd>{setup.init}</Cmd>
              <Comment>no browser on this machine?</Comment>
              <Cmd>{setup.initHeadless}</Cmd>
              <Comment>already set up with another space?</Comment>
              <Cmd>{setup.add}</Cmd>
            </TermFrame>
          }
        >
          It signs in through your browser, then checks the account can read this space and the embedding model.
        </Step>
        <Step
          title="Use it"
          aside={
            <>
              <TermFrame title="agent host" copy={setup.use}>
                {setup.use.split("\n").map((cmd) => (
                  <Cmd key={cmd}>{cmd}</Cmd>
                ))}
              </TermFrame>
              <TermFrame title="mcp.json" copy={setup.mcp} className="stacked">
                <Json text={setup.mcp} />
              </TermFrame>
            </>
          }
        >
          From a shell, or from an MCP client, which gets <code>remember</code>, <code>recall</code> and{" "}
          <code>list_spaces</code> tools.
        </Step>
      </Steps>
      {model ? (
        <p className="muted small">
          Agents embed memories with this space's model, <strong>{model.model}</strong>, through Ollama on their machine.
        </p>
      ) : (
        <p className="muted small">This space hasn't declared an embedding model yet; agents can't store memories until it does.</p>
      )}
    </section>
  );
}
