import { Snippet } from "../components/Snippet";
import { agentSetup } from "../lib/connect";

const REPO = "https://github.com/haileyok/engram-garden";
const EXAMPLE_SPACE = "at://did:plc:you/space/garden.engram.space/memory";

// Landing explains what Engram Garden is to someone who has just arrived. It
// sits under the sign-in box on the signed-out page.
export function Landing() {
  const setup = agentSetup(EXAMPLE_SPACE);
  return (
    <>
      <section className="section" aria-labelledby="what">
        <h2 id="what">What it does</h2>
        <div className="feature-grid">
          <div className="card feature">
            <h3>Remember</h3>
            <p>
              An agent saves a note in plain words, with tags and where it came from. From a shell that's{" "}
              <code>engram remember</code>; inside an agent it's the <code>remember</code> tool over MCP.
            </p>
          </div>
          <div className="card feature">
            <h3>Recall</h3>
            <p>
              Searches go by meaning, not keywords. Ask "how do we ship a change?" and find the note that says
              "deploys go through the deploy repo's workflow".
            </p>
          </div>
          <div className="card feature">
            <h3>Share</h3>
            <p>
              A memory space is shared by the agents you add to it. What one agent writes, the others can find.
              Every memory shows who wrote it, and an agent can delete only its own.
            </p>
          </div>
        </div>
      </section>

      <section className="section" aria-labelledby="see">
        <h2 id="see">Two commands</h2>
        <p className="muted">
          This is the whole interface. Your agent gets the same two as MCP tools, so it can use them on its own.
        </p>
        <pre className="terminal" aria-label="Example terminal session">
          <code>
            <span className="prompt">$ </span>
            <span className="cmd">
              engram remember "Deploys go through the deploy repo's workflow: one SHA, every service." -t infra
            </span>
            {"\n"}
            <span className="out">Remembered: at://did:plc:…/garden.engram.memory/3mxd…</span>
            {"\n\n"}
            <span className="prompt">$ </span>
            <span className="cmd">engram recall "how do we ship a change?"</span>
            {"\n"}
            <span className="out">
              {"[702] 2026-10-08 08:01  did:plc:…  in memory\ntags: infra\nDeploys go through the deploy repo's workflow: one SHA, every service."}
            </span>
          </code>
        </pre>
        <p className="muted small">
          Example output. The number in brackets is how close the match is, from 0 to 1000. Matching is by meaning, so
          a question doesn't need to reuse the note's words.
        </p>
      </section>

      <section className="section" id="how" aria-labelledby="how-h">
        <h2 id="how-h">How it works</h2>
        <ol className="flow" aria-label="How a memory travels">
          <li className="flow-step">
            <strong>An agent</strong>
            <span>turns the note into a vector on its own machine</span>
          </li>
          <li className="flow-arrow" aria-hidden>
            <span>remember</span>
          </li>
          <li className="flow-step">
            <strong>Your memory space</strong>
            <span>a private ATProto space; the note is a record in the writer's account</span>
          </li>
          <li className="flow-arrow" aria-hidden>
            <span>verified</span>
          </li>
          <li className="flow-step">
            <strong>The appview</strong>
            <span>checks each record and keeps the search index</span>
          </li>
          <li className="flow-arrow" aria-hidden>
            <span>recall</span>
          </li>
          <li className="flow-step">
            <strong>Any member agent</strong>
            <span>searches by meaning and gets the closest notes</span>
          </li>
        </ol>
        <ol className="plain steps-text">
          <li>
            <strong>Agents embed their own notes.</strong> Each memory is stored with a vector made by the embedding
            model the space declares, run on the agent's machine (Ollama by default). The search service never runs a
            model.
          </li>
          <li>
            <strong>The space keeps it private.</strong> A memory space is a private{" "}
            <a href="https://github.com/bluesky-social/atproto/pull/5187">ATProto space</a>. Only the accounts its owner
            adds can read it.
          </li>
          <li>
            <strong>The appview indexes it.</strong> Once the owner lets it, a search service called the appview reads
            each member's new records, checks every one against the member's signed commit, and adds it to that space's
            index.
          </li>
          <li>
            <strong>Members search.</strong> An agent embeds its question the same way and asks the appview for the
            nearest notes. The appview answers only accounts that are members of the space.
          </li>
        </ol>
      </section>

      <section className="section" aria-labelledby="start">
        <h2 id="start">Get started</h2>
        <ol className="connect">
          <li>
            <strong>Sign in above and create a space.</strong> Pick a name and an embedding model, then let the appview
            index it (you approve read-only access on your account's own sign-in page).
          </li>
          <li>
            <strong>Install the tools</strong> (with Nix, for now) on the machine your agent runs on. They need{" "}
            <a href="https://ollama.com">Ollama</a> for embeddings:
            <Snippet text={setup.install} />
          </li>
          <li>
            <strong>Sign the agent in</strong> to the space. It can use your account or have its own, which you add to
            the space as a member who can write:
            <Snippet text={setup.init} />
          </li>
          <li>
            <strong>Add the MCP server</strong> to your MCP client's config. The agent then has <code>remember</code>{" "}
            and <code>recall</code>:
            <Snippet text={setup.mcp} />
          </li>
        </ol>
        <p className="muted small">
          Each space's page has a <strong>Connect an agent</strong> tab with these commands filled in for that space.
        </p>
      </section>

      <section className="section" aria-labelledby="trust">
        <h2 id="trust">What you're trusting</h2>
        <ul className="trust">
          <li>
            <strong>Your memories stay in ATProto accounts you or your agents control.</strong> The appview keeps only a
            search index, which can be rebuilt from those records.
          </li>
          <li>
            <strong>The appview can read every memory in a space you let it index.</strong> It gets read-only access,
            once, from the space's owner, and the owner can take it back from the space's page. Don't store secrets in a
            memory space.
          </li>
          <li>
            <strong>The index holds only what members wrote.</strong> Every change is checked against its author's
            signed commit before it's indexed, and data that doesn't check out is never indexed.
          </li>
          <li>
            <strong>Search is for members only.</strong> Callers present a signed credential for the space, the same
            proof the space's host asks for.
          </li>
          <li>
            <strong>You can run your own.</strong> The code is public, appview included.{" "}
            <a href={`${REPO}#running-the-appview`}>Here's how</a>.
          </li>
        </ul>
      </section>

      <section className="section cta" aria-label="Sign in">
        <h2>Give your agents something to remember with.</h2>
        <a href="#signin" className="button primary">Sign in</a>
      </section>

      <footer className="landing-foot">
        <a href={REPO}>Source on GitHub</a>
        <a href={`${REPO}/blob/main/docs/design/storage.md`}>How the index is stored</a>
        <a href={`${REPO}/blob/main/docs/design/indexing-access.md`}>How the appview gets access</a>
      </footer>
    </>
  );
}
