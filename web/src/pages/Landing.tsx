import type { ReactNode } from "react";
import { AgentDemo } from "../components/AgentDemo";
import { Prompt, TermFrame } from "../components/Terminal";
import { Logo } from "../Logo";
import { agentSetup } from "../lib/connect";

const REPO = "https://github.com/haileyok/engram-garden";
const EXAMPLE_SPACE = "at://did:plc:you/space/garden.engram.space/memory";

function Section({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <section className="sec" aria-labelledby={id}>
      <h2 id={id}>{title}</h2>
      <div className="sec-body">{children}</div>
    </section>
  );
}

// A thin rule with a sprout in the middle, between sections.
function Sprig() {
  return (
    <div className="sprig" aria-hidden>
      <Logo size={16} />
    </div>
  );
}

// Colors the keys and strings of JSON for display in a terminal window.
function Json({ text }: { text: string }) {
  const parts: ReactNode[] = [];
  const re = /("(?:[^"\\]|\\.)*")(\s*:)?/g;
  let last = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    parts.push(text.slice(last, m.index));
    parts.push(
      <span key={m.index} className={m[2] ? "t-key" : "t-str"}>
        {m[1]}
      </span>,
    );
    if (m[2]) parts.push(m[2]);
    last = m.index + m[0].length;
  }
  parts.push(text.slice(last));
  return <>{parts}</>;
}

// Landing explains what Engram Garden is. It sits under the sign-in box on the
// signed-out page.
export function Landing() {
  const setup = agentSetup(EXAMPLE_SPACE);
  return (
    <>
      <figure className="demo">
        <AgentDemo />
        <figcaption>Two agents on different machines. The second finds the note the first saved. Example output.</figcaption>
      </figure>

      <Sprig />

      <Section id="how" title="How it works">
        <p>
          A memory space is a private ATProto space. Its owner chooses the member accounts, and each agent is one of
          them.
        </p>
        <p>
          When an agent saves a note, it turns the text into a vector with the embedding model the space declares. That
          runs on the agent's own machine, through Ollama by default. The text and the vector are stored as a record in
          the agent's account.
        </p>
        <p>
          A search service called the appview indexes those records once the space's owner allows it. It checks each
          change against the author's signed commit before it goes into the index.
        </p>
        <p>
          To search, an agent turns its question into a vector the same way and asks the appview for the closest notes.
          The appview only answers members of the space.
        </p>
      </Section>

      <Sprig />

      <Section id="setup" title="Set up an agent">
        <p>
          Sign in above, create a space, and let the appview index it. You approve read-only access on your account's
          own sign-in page. Then, on the machine the agent runs on (it needs Nix and Ollama):
        </p>
        <TermFrame title="agent host" copy={`${setup.install}\n${setup.init}`}>
          <div className="t-line">
            <Prompt cwd="~" />
            {setup.install}
          </div>
          <div className="t-line">
            <Prompt cwd="~" />
            {setup.init}
          </div>
        </TermFrame>
        <p>
          The agent can use your account or have its own, which you add to the space as a member who can write. To give
          it the tools, add the MCP server to its client's config:
        </p>
        <TermFrame title="mcp.json" copy={setup.mcp}>
          <Json text={setup.mcp} />
        </TermFrame>
        <p className="muted">Each space's page has a Connect an agent tab with these commands filled in for it.</p>
      </Section>

      <Sprig />

      <Section id="trust" title="What the appview can see">
        <p>
          Memories are records in ATProto accounts that you or your agents control. The index the appview keeps can be
          rebuilt from them.
        </p>
        <p>
          The appview can read every memory in a space you let it index. Its access is read-only, you grant it once, and
          you can withdraw it from the space's page. Don't store secrets in a memory space.
        </p>
        <p>
          Everything it indexes is first checked against its author's signed commit, and only members can search. The
          code is public, appview included, if you want to run your own: see{" "}
          <a href={`${REPO}#running-the-appview`}>running the appview</a>.
        </p>
      </Section>

      <footer className="landing-foot">
        <a href={REPO}>Source on GitHub</a>
        <a href={`${REPO}/blob/main/docs/design/storage.md`}>How the index is stored</a>
        <a href={`${REPO}/blob/main/docs/design/indexing-access.md`}>How the appview gets access</a>
        <a href="#signin">Sign in</a>
      </footer>
    </>
  );
}
