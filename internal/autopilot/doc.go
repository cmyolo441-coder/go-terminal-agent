// Package autopilot drives the agent through structured, verifiable work.
//
// It provides two workflows and one tool:
//
//   - Runner, a plan-then-execute pipeline. A read-only planner agent
//     investigates the workspace and emits a structured Plan (numbered steps,
//     files to touch, risks and a verification strategy) as fenced JSON. A
//     separate reviewer agent with a fresh context critiques the plan against
//     the task and the code; the planner revises until the reviewer approves
//     or the round cap is reached. The user approves, rejects or sends the
//     plan back with feedback through a callback. The approved plan is handed
//     to the main agent, whose todo list tracks step progress, and finally a
//     verifier checks the actual workspace diff (and optionally the test
//     suite) against the plan and reports gaps, which can be fed back for a
//     bounded number of fix rounds.
//
//   - FixTests, an automatic test-and-fix loop. It detects the project's test
//     command (Go, npm/pnpm/yarn/bun, pytest, cargo, Maven, Gradle, make),
//     runs it with a timeout, parses the output into structured failures
//     (file, line, test, message) for Go, pytest, Jest, Vitest, Cargo and
//     JVM runners, feeds the most relevant failures to the agent within a
//     small budget, and repeats until the suite is green, the iteration cap
//     is hit, two rounds pass without progress, or the context is cancelled.
//
//   - VerifyTool, a "verify" tool the model can call to run the detected test
//     command itself, optionally filtered to matching tests, and get the same
//     structured failure summary.
//
// Every model call made by helper agents is reported for billing, every
// phase is observable through callbacks, and every loop is bounded.
package autopilot
