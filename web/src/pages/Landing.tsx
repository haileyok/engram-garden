import type { ReactNode } from "react";
import { Garden } from "../components/Garden";
import { Step, Steps } from "../components/Steps";
import { Json, Prompt, TermFrame } from "../components/Terminal";
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

function Section({
  id,
  title,
  lede,
  children,
}: {
  id: string;
  title: string;
  lede?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="sec" aria-labelledby={id}>
      <h2 id={id}>{title}</h2>
      {lede && <p className="sec-lede">{lede}</p>}
      {children}
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

// Three things to know before reading on, under the garden.
function Promises() {
  return (
    <div className="promises">
      <div className="promise">
        <h2>On any machine</h2>
        <p>Write a note on your laptop. Find it from the CI box, the server, the Pi in the closet.</p>
      </div>
      <div className="promise">
        <h2>With your whole team</h2>
        <p>
          Everyone on a project writes to the same space, and everyone's agents can search it. Scripts and teammates can
          add notes too.
        </p>
      </div>
      <div className="promise">
        <h2>Yours to keep</h2>
        <p>
          Notes are records in your own ATProto account. The search index is built from those records, so you can
          rebuild it or run your own.
        </p>
      </div>
    </div>
  );
}

// Landing explains what Engram Garden is. It sits under the sign-in box on the
// signed-out page.
export function Landing() {
  const setup = agentSetup(EXAMPLE_SPACE);
  const own = agentSetup(EXAMPLE_SPACE, OWN_APPVIEW);
  return (
    <>
      <Garden />
      <p className="garden-fine">
        Example notes and a simplified matcher, to show the idea. Real searches embed the question with the space's
        embedding model.
      </p>

      <Promises />

      <Sprig />

      <Section id="team" title="Shared across a team">
        <div className="cards">
          <div className="card">
            <h3>Scripts can write too</h3>
            <p>
              A script can save a note with <code>engram remember</code>, so you can record every merge. The agents in
              the space can search that history.
            </p>
          </div>
          <div className="card">
            <h3>Everyone reads everything</h3>
            <p>
              Everyone in a space can read all of it, though members can be read-only. Keep a separate space for
              anything that shouldn't go to the whole team.
            </p>
          </div>
        </div>
      </Section>

      <Sprig />

      <Section
        id="how"
        title="How it works"
        lede="A memory space is a private ATProto space. Its owner chooses the member accounts, and each agent works as one of them."
      >
        <ol className="flow">
          <li>
            <h3>Save</h3>
            <p>
              The agent embeds the note with the model the space declares, on its own machine through Ollama by default.
              The text and vector are stored as a record in its account.
            </p>
          </li>
          <li>
            <h3>Index</h3>
            <p>
              Once the space's owner grants it read-only access, the appview indexes those records. Each change is
              checked against the author's signed commit first.
            </p>
          </li>
          <li>
            <h3>Search</h3>
            <p>
              An agent embeds its question the same way and asks the appview for the closest notes. Only members of the
              space get an answer.
            </p>
          </li>
        </ol>
      </Section>

      <Sprig />

      <Section id="setup" title="Set up an agent">
        <Steps>
          <Step title="Create a space">
            Sign in above, create a space, and let the appview index it. You approve its read-only access on your
            account's own sign-in page.
          </Step>
          <Step
            title="Install on the agent's machine"
            aside={
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
            }
          >
            It needs Nix and Ollama.
          </Step>
          <Step
            title="Give it the tools"
            aside={
              <TermFrame title="mcp.json" copy={setup.mcp}>
                <Json text={setup.mcp} />
              </TermFrame>
            }
          >
            The agent can use your account or have one of its own, which you add to the space as a member who can write.
            Add the MCP server to its client's config.
          </Step>
        </Steps>
      </Section>

      <Sprig />

      <Section id="selfhost" title="Run your own appview">
        <Steps>
          <Step
            title="Start it"
            aside={
              <TermFrame title="your server" copy={START_APPVIEW.join("\n")}>
                {START_APPVIEW.map((cmd) => (
                  <div className="t-line" key={cmd}>
                    <Prompt cwd="~" />
                    {cmd}
                  </div>
                ))}
              </TermFrame>
            }
          />
          <Step
            title="Point an agent at it"
            aside={
              <TermFrame title="agent host" copy={own.init}>
                <div className="t-line">
                  <Prompt cwd="~" />
                  {own.init}
                </div>
              </TermFrame>
            }
          >
            Add <code>--appview</code> with your address when the agent signs in.
          </Step>
        </Steps>

        <dl className="facts">
          <div>
            <dt>Indexing a space</dt>
            <dd>
              A space's owner grants your appview access the same way they would ours, on their own account's sign-in
              page.
            </dd>
          </div>
          <div>
            <dt>Storage</dt>
            <dd>
              A local directory is enough for a trial. With no database it forgets its grants whenever it restarts. For
              something that lasts, give it an S3-compatible bucket and a Postgres.
            </dd>
          </div>
          <div>
            <dt>Public address</dt>
            <dd>
              Give it a public HTTPS address so account servers can tell it when a note is written. On localhost it
              polls every five minutes instead.
            </dd>
          </div>
          <div>
            <dt>Moving a space</dt>
            <dd>
              Your notes live in the members' accounts, so they stay put. The new appview rebuilds the index from those
              records, or you can export the old index and load it with <code>engram-appview import</code>.
            </dd>
          </div>
        </dl>
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
