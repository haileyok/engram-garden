import type { ReactNode } from "react";
import { AgentDemo } from "../components/AgentDemo";
import { Prompt, TermFrame } from "../components/Terminal";
import { Logo } from "../Logo";
import { agentSetup } from "../lib/connect";

const REPO = "https://github.com/haileyok/engram-garden";
const EXAMPLE_SPACE = "at://did:plc:you/space/garden.engram.space/memory";
const OWN_APPVIEW = "https://memory.example.com";

// Starting an appview of your own, for the self-hosting section.
const START_APPVIEW = [
  "git clone https://github.com/haileyok/engram-garden && cd engram-garden",
  "goat key generate -t P-256",
  [
    "ENGRAM_SERVICE_DID=did:web:memory.example.com \\",
    `  ENGRAM_PUBLIC_URL=${OWN_APPVIEW} \\`,
    "  ENGRAM_OAUTH_KEY=<the key from above> \\",
    "  go run ./cmd/engram-appview",
  ].join("\n"),
];

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

// The loud part of the page: why this is built on ATProto. The statements are
// big on purpose, and the small print under each says what's behind it.
function WhyAtproto() {
  return (
    <section className="band" aria-labelledby="why">
      <h2 id="why">Why it's built on ATProto</h2>
      <ul className="why">
        <li>
          <p className="why-line">Your account is the database.</p>
          <p className="why-note">
            Every note is a <code>garden.engram.memory</code> record in your own repo.
          </p>
        </li>
        <li>
          <p className="why-line">One login on every machine.</p>
          <p className="why-note">The same handle signs you into the web app, the CLI and each of your agents.</p>
        </li>
        <li>
          <p className="why-line">Anyone can run the index.</p>
          <p className="why-note">The appview is a server that reads your records and builds a search index over them. You can run one yourself.</p>
        </li>
        <li>
          <p className="why-line">Every record is signed.</p>
          <p className="why-note">Each change is checked against its author's signed commit before it enters the index.</p>
        </li>
      </ul>
    </section>
  );
}

// Landing explains what Engram Garden is. It sits under the sign-in box on the
// signed-out page.
export function Landing() {
  const setup = agentSetup(EXAMPLE_SPACE);
  const own = agentSetup(EXAMPLE_SPACE, OWN_APPVIEW);
  return (
    <>
      <figure className="demo">
        <AgentDemo />
        <figcaption>
          Two agents on two machines, one shared space. The second finds the note the first saved. Example output.
        </figcaption>
      </figure>

      <WhyAtproto />

      <Sprig />

      <Section id="how" title="How it works">
        <p>
          A memory space is a private ATProto space. Its owner chooses the member accounts, and each agent works as one
          of them.
        </p>
        <p>
          When an agent saves a note, it turns the text into a vector with the embedding model the space declares. The
          embedding runs on the agent's own machine, through Ollama by default. The text and the vector go into a
          record in the agent's account.
        </p>
        <p>
          A search service called the appview indexes those records once the space's owner grants it read-only access.
          Nothing goes into the index until it has been checked against the author's signed commit.
        </p>
        <p>
          To search, an agent embeds its question the same way and asks the appview for the closest notes. The appview
          only answers members of the space.
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
        <p className="muted">Each space's page has a Connect an agent tab with these commands filled in for that space.</p>
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
          Everything it indexes is first checked against its author's signed commit, and only members can search.
        </p>
      </Section>

      <Sprig />

      <Section id="selfhost" title="Run your own appview">
        <p>
          The appview is a Go program in this repo. Point it at a local directory and it runs without a database. It
          forgets its grants whenever it restarts, which is fine for a trial. For something that lasts, give it an
          S3-compatible bucket and a Postgres.
        </p>
        <TermFrame title="your server" copy={START_APPVIEW.join("\n")}>
          {START_APPVIEW.map((cmd) => (
            <div className="t-line" key={cmd}>
              <Prompt cwd="~" />
              {cmd}
            </div>
          ))}
        </TermFrame>
        <p>Then point an agent at it:</p>
        <TermFrame title="agent host" copy={own.init}>
          <div className="t-line">
            <Prompt cwd="~" />
            {own.init}
          </div>
        </TermFrame>
        <p>
          A space's owner lets your appview index it the same way they would ours, on their own account's sign-in page.
          Moving a space later doesn't move your notes, since they live in the members' accounts. The new appview
          rebuilds the index from those records, or you can export the old index and load it with{" "}
          <code>engram-appview import</code>.
        </p>
        <p>
          For a deployment other people will use, give it a public HTTPS address so account servers can tell it when a
          note is written. On localhost it polls every five minutes instead. Every setting is in the{" "}
          <a href={`${REPO}#running-the-appview`}>README</a>.
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
