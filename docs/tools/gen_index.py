#!/usr/bin/env python3
"""Generate Appendix A (the requirements index) of SPEC.md.

Usage:
    python3 docs/tools/gen_index.py SPEC.md               check the rows, then rewrite Appendix A
    python3 docs/tools/gen_index.py SPEC.md --check-only  check only; fail if SPEC.md is out of date

The rows in ROWS are the source of Appendix A. To add or change a requirement, edit ROWS and run
the script; do not edit the tables in SPEC.md by hand, because the next run overwrites them.

Each row is (id, requirement, sections, milestone, strength, tests, anchor, note):

- id         stable identifier; never renumber an existing one
- sections   sections of SPEC.md that state the rule
- milestone  first milestone in which the rule has to hold (see MILESTONES)
- strength   MUST, SHOULD, or MAY; it has to agree with the sentence in the body
- tests      test items of section 19 that name the behaviour
- anchor     phrase that must appear in one of the cited sections; it is not written to SPEC.md.
             When the body is reworded and the phrase disappears, the check fails, which is the
             signal to review the row.
- note       why the milestone was a judgement call, or None; it is not written to SPEC.md

The script also gives every deterministic test item of section 19 an identifier. Existing
identifiers are kept, and a new item gets the next unused number, wherever it is inserted.
"""
import re
import sys

AREAS = [
    ("ROLE", "Roles"),
    ("FSM", "State model and gates"),
    ("HITL", "Human in the loop"),
    ("EFF", "Effort"),
    ("ART", "Artifact contracts"),
    ("VER", "Verification"),
    ("SCH", "Strategies and scheduling"),
    ("RES", "Resource coordination"),
    ("INT", "Workspace integration"),
    ("RUN", "Agent runtime"),
    ("PROC", "Process supervision"),
    ("PER", "Persistence"),
    ("REC", "Recovery and reconciliation"),
    ("CTX", "Context"),
    ("MAN", "Run manifest"),
    ("WS", "Workspace"),
    ("CFG", "Configuration"),
    ("ERR", "Errors and retries"),
    ("BUD", "Budgets"),
    ("CLI", "CLI and observability"),
    ("TECH", "Technology and evaluation"),
]

M, S_, Y = "MUST", "SHOULD", "MAY"
PM = "Post-MVP"

ROWS = [
    # ---------------- Roles (section 4)
    ("R-ROLE-01", "Launch the Coordinator only when reasoning is needed; it is not a continuously running parent agent.", ["4"], "M2", M, [], "The Coordinator is not a continuously running parent agent", None),
    ("R-ROLE-02", "Let agents report completion, but grant acceptance only in the engine, after it has verified the required evidence.", ["4"], "M1b", M, [], "The engine verifies the required evidence before granting acceptance", None),
    ("R-ROLE-03", "Have the Evaluator exercise the software, for example by running it or its tests, and not only read the diff.", ["4"], "M3", S_, [], "The Evaluator SHOULD exercise the software", None),
    # ---------------- State model (section 6)
    ("R-FSM-01", "Track the state of the Run, of each Phase, and of each Attempt separately.", ["6"], "M0", M, ["T-01"], "Bellona tracks three separate state machines", None),
    ("R-FSM-02", "Reject every transition that the Run, Phase, and Attempt tables do not list.", ["6"], "M0", M, ["T-01"], "The engine rejects every transition that is not listed", None),
    ("R-FSM-03", "Let only the Workflow Engine perform state transitions and artifact acceptance (invariant 9).", ["6"], "M0", M, [], "Only the Workflow Engine may perform state transitions", None),
    ("R-FSM-04", "Do not start implementation before an accepted plan exists (invariant 1).", ["6"], "M1b", M, [], "Do not start implementation until an accepted plan exists", None),
    ("R-FSM-05", "Complete a Run only after every Phase is accepted and final verification succeeds (invariant 7).", ["6"], "M1b", M, [], "Complete a Run only after all phases are accepted", None),
    ("R-FSM-06", "Stop when a retry, replan, time, or other permitted limit is exceeded (invariant 8).", ["6"], "M1a", M, ["T-01", "T-15"], "Stop when retry, replan, time, cost", None),
    ("R-FSM-07", "Record the state to resume when a Run is suspended, and keep no agent process running while it is suspended.", ["6"], "M2", M, ["T-02", "T-07"], "are suspended states: the engine records the state to resume", None),
    ("R-FSM-08", "Offer approve, request changes, and reject for plan approval; a change request does not consume the replan counter.", ["6"], "M2", M, ["T-02"], "Plan approval offers three responses", None),
    ("R-FSM-09", "Return to planning after a final verification or final evaluation failure, with repair Phases in a new Plan Revision.", ["6"], "M3", M, ["T-02"], "A final verification or final evaluation failure has no single owning Phase", None),
    ("R-FSM-10", "Keep a Phase at its retry limit waiting in `Recovering`, and block the Run only when no Phase can make progress and replanning is not authorized.", ["6"], "M3", M, [], "The Run becomes `Blocked` only when no Phase can make progress", None),
    ("R-FSM-11", "On replanning or cancellation, stop new dispatches and terminate and reconcile active Attempts before committing the change (invariant 16).", ["6"], "M3", M, ["T-20"], "On replanning or cancellation, stop new dispatches", None),
    ("R-FSM-12", "Never rewind the integration ref when replanning; plan the new revision against the current integration commit.", ["6"], "M3", M, ["T-19"], "Replanning never rewinds the integration ref", None),
    ("R-FSM-13", "Carry acceptance over to a new revision only when the Phase ID, its definition digest, and the digests of its transitive dependencies are unchanged; other accepted Phases become `Superseded`.", ["6"], "M3", M, ["T-02"], "keeps its `Accepted` state only if", None),
    ("R-FSM-14", "Treat `Blocked` as resumable, and preserve accepted Phases, their evidence, and the integration ref.", ["6.3"], "M3", M, ["T-02"], "`Blocked` is a suspended state, not a terminal one", None),
    ("R-FSM-15", "Accept a policy amendment only from the user through the CLI and only for execution limits, and record it as a `PolicyAmended` event without rewriting the frozen files.", ["6.3"], "M3", M, ["T-02"], "is appended to the journal as a `PolicyAmended` event", None),
    ("R-FSM-16", "Do not equate an uncertain external outcome with failure, and do not repeat the work before reconciliation is complete (invariant 11).", ["6"], "M3", M, ["T-12"], "Do not equate an uncertain external execution outcome with failure", None),
    ("R-FSM-17", "Start a Phase only after all its dependencies are accepted and integrated (invariants 2 and 15).", ["6"], "M4", M, ["T-01", "T-18"], "A Phase may start only after all its dependencies have been accepted", None),
    ("R-FSM-18", "Prohibit concurrent Attempts for the same Phase or session, and manage slots and state changes in a single control loop (invariant 14).", ["6"], "M1b", M, ["T-18"], "Prohibit concurrent Attempts for the same Phase or Session", None),
    ("R-FSM-19", "Require the user's approval of a valid plan by default, and let `execution.requirePlanApproval` turn it off.", ["14"], "M2", M, [], "`execution.requirePlanApproval`, `true` by default", None),
    # ---------------- HITL (sections 6.1, 6.2, 8)
    ("R-HITL-01", "Accept a question from any role only as the common `needs_user_input` output, and never treat a question request as successful implementation.", ["8"], "M2", M, [], "This output is the only way for any role to ask the user", None),
    ("R-HITL-02", "Assign each request an ID that is unique within the Run, and bind it to the originating role, Attempt, session, plan revision, and resume target.", ["8"], "M2", M, ["T-07"], "The engine assigns a Request ID unique within the Run", None),
    ("R-HITL-03", "When a question is accepted, stop new dispatches across the Run, let running Attempts finish, and commit `WaitingForUser` only after every execution process is reaped.", ["6.1"], "M2", M, ["T-07", "T-20"], "Commit `WaitingForUser` only after all execution processes are reaped", None),
    ("R-HITL-04", "In non-interactive mode, persist the questions, reap all processes, and exit with a dedicated exit code so that answers and resumption can follow later.", ["6.1"], "M2", M, ["T-07"], "Use a dedicated exit code to signal pending input", None),
    ("R-HITL-05", "Do not charge waiting for input to failure, retry, or replan counts, and measure waiting time separately from active time.", ["6.1"], "M2", M, ["T-15"], "does not consume failure, retry, or replan counts", None),
    ("R-HITL-06", "Resume the originating session as the next Turn of the same Attempt; if the session cannot be restored, mark the Attempt `Abandoned` and continue in a new Attempt with the handoff context, or stop as `Blocked` when a handoff input is missing.", ["6.1"], "M2", M, ["T-07", "T-16"], "The resumed execution is the next Turn of the same Attempt", None),
    ("R-HITL-07", "Have the Planner interpret answers into the plan; never pass an answer to the Implementer in a way that bypasses plan validation.", ["6.1"], "M2", M, [], "bypass plan validation", None),
    ("R-HITL-08", "Do not start work that depends on an unanswered mandatory question, never fill in answers automatically, and never treat a recommendation as an answer (invariant 10).", ["6", "6.1"], "M2", M, ["T-07"], "Do not fill in unanswered questions automatically", None),
    ("R-HITL-09", "Append Other to every choice question at presentation time, reject an agent option with the reserved ID `other`, and store free text verbatim.", ["8"], "M2", M, ["T-07"], "Bellona always appends Other at presentation time", None),
    ("R-HITL-10", "Validate answers against the contract of the question type, and reject input for answered, invalidated, or other-Run questions.", ["8"], "M2", M, ["T-07"], "reject input for already answered, invalidated, or different-Run questions", None),
    ("R-HITL-11", "Bind an approval to the hash of the plan or artifact, and never reuse it for a modified version.", ["6.2"], "M2", M, ["T-17"], "Bind approval to the hash of the relevant plan or artifact", None),
    ("R-HITL-12", "On resume, check whether the worktree or the plan assumptions changed; do not rebase a Run automatically, and require reassessment when the change affects execution.", ["6.2"], "M2", M, ["T-17"], "Do not automatically rebase an existing Run", None),
    ("R-HITL-13", "Record cancellation and invalidation of questions as events, and never overwrite an existing answer with different content.", ["6.2"], "M2", M, ["T-07", "T-17"], "Record question cancellation and invalidation as events", None),
    ("R-HITL-14", "Pass free text to the next agent as user requirements; never execute it as shell arguments or let it change engine constraints.", ["8"], "M2", M, [], "Pass free text to the next agent as user requirements", None),
    ("R-HITL-15", "Have the Planner ask the user only when user intent needs clarification, not for matters it can investigate or decide itself.", ["6.1"], "M2", M, [], "It must not interrupt the user", None),
    # ---------------- Effort (section 7)
    ("R-EFF-01", "Select the model and the effort separately, and do not require the user to specify effort for every Run.", ["7"], "M2", M, [], "Separate model selection from effort selection", None),
    ("R-EFF-02", "Offer the LLM only effort values that the adapter supports and the policy permits.", ["7"], "M2", M, [], "Present only selectable options to the LLM", None),
    ("R-EFF-03", "Record the requested value, the value passed to the adapter, and the value confirmed by the host separately, for the effort and for the model; record an unreported value as unknown.", ["7"], "M2", M, [], "Record the requested value, the value passed to the adapter", None),
    ("R-EFF-04", "When the host cannot set the effort, or cannot change it on resume, do not claim that it was applied; follow an explicit fallback policy.", ["7"], "M2", M, [], "Follow an explicit fallback policy", None),
    ("R-EFF-05", "Take the Evaluator's effort from the Planner's proposal in the plan; do not launch the Coordinator to start an Evaluator.", ["7"], "M3", M, [], "Starting an Evaluator does not require a Coordinator launch", None),
    # ---------------- Artifact contracts (section 8)
    ("R-ART-01", "Validate artifacts with typed structures, and reject unknown fields, trailing values, missing required fields, invalid nulls and values, and duplicate keys.", ["8"], "M0", M, ["T-03"], "reject unknown fields, trailing JSON values", None),
    ("R-ART-02", "Keep engine-assigned identifiers out of agent output; write an Attempt binding with the launch intent, and accept an output only when its binding matches the current Plan Revision and Attempt (invariant 4).", ["8"], "M1b", M, ["T-03"], "The engine writes an Attempt binding when it persists launch intent", None),
    ("R-ART-03", "Validate plans for unique IDs, existing references, acyclic dependencies, acceptance criteria, permitted effort values, verification references, and repository-relative scopes.", ["8"], "M2", M, ["T-01"], "Plan validation checks ID uniqueness", None),
    ("R-ART-04", "Require every acceptance criterion to name its verification means in `verifiedBy`, and reject a criterion that has none or names an unavailable one.", ["8"], "M2", M, ["T-09"], "Every acceptance criterion names how it is verified", None),
    ("R-ART-05", "Require every step file to lie within the change scope, and let only the engine assign the Plan Revision number.", ["8"], "M2", M, [], "every step file must lie within the change scope", None),
    ("R-ART-06", "Match change scopes with the Bellona pattern grammar, and reject absolute paths, `..` segments, and negation.", ["8"], "M1b", M, ["T-06"], "Absolute paths, `..` segments, and negation are rejected", None),
    ("R-ART-07", "Treat `implemented` as a report only, and compare the reported changed files with the actual diff.", ["8"], "M1b", M, ["T-03"], "The engine obtains the actual diff and compares it with the report", None),
    ("R-ART-08", "Reject an evaluation that omits an acceptance criterion or whose verdict disagrees with its criteria and findings.", ["8"], "M3", M, ["T-09"], "The engine rejects an evaluation that omits a criterion", None),
    ("R-ART-09", "Bind each review to the Implementer Attempt, the candidate commit, and the tree, so that it cannot be reused for another version.", ["8"], "M3", M, ["T-03"], "The engine binds the review to the Implementer Attempt", None),
    ("R-ART-10", "Treat a Coordinator decision as a proposal, and execute it only after validating state, target Phase, permissions, budget, and capabilities.", ["8"], "M2", M, [], "A decision is a proposal, not direct command execution", None),
    # ---------------- Verification (sections 6, 9, 10)
    ("R-VER-01", "Verify independently of the agent's report, and never grant acceptance when a mandatory check failed, did not run, or is stale (invariant 5).", ["9"], "M1b", M, [], "Perform verification independently of the agent's self-reported results", None),
    ("R-VER-02", "Run only named checks that are resolved from trusted definitions; never execute a shell string that an LLM generated.", ["9"], "M1b", M, [], "Do not execute arbitrary LLM-generated shell strings", None),
    ("R-VER-03", "Record in the evidence the command identifier, exit code, timeout, log references, and target identifiers.", ["9"], "M1b", M, [], "Verification evidence records the command identifier", None),
    ("R-VER-04", "Run the required checks on the base commit at Run start, and block the Run before any agent launch when this baseline fails.", ["9"], "M1b", M, ["T-06"], "run the required checks once on the base commit", None),
    ("R-VER-05", "Record the files that the checks create in the baseline run, and treat them as ignored for the rest of the Run.", ["9"], "M2", M, ["T-10"], "The baseline run also records the files that the checks create", None),
    ("R-VER-06", "Give the Implementer the commands of its Phase's checks; do not use its own runs as evidence, and do not treat a refused self-check as a failure.", ["9"], "M2", M, ["T-10"], "Give the Implementer the commands of its Phase's checks", None),
    ("R-VER-07", "Treat a change to, or the deletion of, an existing protected file as a scope violation unless a step of the Phase names the file with that action.", ["9"], "M1b", M, ["T-09"], "unless a step of the Phase names that file with the matching action", None),
    ("R-VER-08", "When review is mandatory, require an accepted verdict from an independent Evaluator before acceptance (invariant 6).", ["6"], "M3", M, [], "an acceptance verdict from an independent Evaluator is required", None),
    ("R-VER-09", "Set the evaluation frequency with `independentReview` (`none`, `final`, `phase`), and keep mechanical and final verification under `none`.", ["10"], "M3", M, [], "Evaluation frequency is set by one setting", None),
    # ---------------- Strategies and scheduling (section 10)
    ("R-SCH-01", "Implement Standard and Phased as presets of the one workflow, and enforce the single Phase of Standard for the first Plan Revision only.", ["10"], "M2", M, ["T-09"], "Standard is the same workflow with an initial plan of exactly one Phase", None),
    ("R-SCH-02", "Implement Strict as Phased with `phase` as a review minimum that configuration cannot lower.", ["10"], "M3", M, [], "Strict is Phased with `phase` as a minimum", None),
    ("R-SCH-03", "Bound the concurrently running agent processes of every role by `maxConcurrency`, and launch at least two agents concurrently when it allows.", ["10"], "M1b", M, ["T-18"], "The MVP requires actual concurrent launches of multiple agents", None),
    ("R-SCH-04", "With `maxConcurrency` of one, keep at most one Phase in flight.", ["10"], "M4", M, ["T-18"], "the Scheduler also keeps at most one Phase in flight", None),
    ("R-SCH-05", "Derive the Ready set from the accepted plan, break ties by plan order and Phase ID, and dispatch a Phase as soon as its dependencies are accepted.", ["10"], "M4", M, ["T-18"], "Break dispatch-order ties by plan order and Phase ID", None),
    ("R-SCH-06", "Reserve a slot when the launch intent is persisted and release it after the process is reaped; a worker that holds a slot never waits for another.", ["10"], "M1b", M, ["T-18"], "Reserve a slot when persisting launch intent", None),
    ("R-SCH-07", "Have workers return results through channels only, and let a single control loop validate results and commit events.", ["10"], "M0", M, ["T-20"], "A single control loop validates candidate results and commits events", None),
    ("R-SCH-08", "Keep the successes of independent Phases when one Phase fails, do not start its dependents, and do not repeat preserved work while its assumptions hold.", ["10"], "M4", M, ["T-20"], "Failure of an independent Phase does not revoke other phases' successes", None),
    ("R-SCH-09", "Stop new dispatches on Run-wide events: replanning, an uncertain outcome, budget exhaustion, a rate-limit rejection, or a question to the user.", ["10"], "M4", M, ["T-20"], "Stop new dispatches on Run-wide events", None),
    ("R-SCH-10", "Accept a new revision only after active Attempts have finished or been cancelled and reconciled, and reject late results from older revisions.", ["10"], "M3", M, ["T-20"], "reject late results from older revisions", None),
    ("R-SCH-11", "Fall back to serial execution when parallel safety cannot be established.", ["10"], "M4", M, ["T-19"], "Fall back to serial execution when safety cannot be established", None),
    ("R-SCH-12", "Optimize session reuse, dynamic concurrency, and advanced resource locks from measurements, while preserving the mandatory gates.", ["18"], "M6", M, [], "Improve execution efficiency based on measurements", None),
    ("R-SCH-13", "Add the Direct strategy and strategy selection by the Coordinator.", ["10"], "M7", M, [], "Direct and strategy selection by the Coordinator are later milestones", None),
    # ---------------- Resource coordination (section 10.1)
    ("R-RES-01", "Check declared resource IDs against trusted configuration, acquire all needed locks together in a fixed order, and dispatch only when they are held.", ["10.1"], "M4", M, ["T-19"], "dispatch only when all necessary locks can be acquired together", None),
    ("R-RES-02", "Hold shared-resource locks until the agent and its verification processes have stopped.", ["10.1"], "M4", M, ["T-19"], "are held until both the agent and the associated verification processes stop", None),
    ("R-RES-03", "Hold change-scope locks from a Phase's first dispatch until it is `Accepted`, `Failed`, or `Superseded`.", ["10.1"], "M4", M, ["T-19"], "are acquired at the Phase's first dispatch", None),
    ("R-RES-04", "Derive lock ownership from Phase state so that replay reconstructs it.", ["10.1"], "M4", M, [], "Lock ownership follows from Phase state", None),
    # ---------------- Workspace integration (section 10.2)
    ("R-INT-01", "Freeze the base commit at Run start, use a dedicated integration ref and workspace, and never apply changes to the user's branch or working tree automatically.", ["10.2"], "M1b", M, ["T-06", "T-19"], "Freeze the base commit at Run start", None),
    ("R-INT-02", "Create a dedicated worktree for each Phase from the current accepted integration commit, and never let two Implementers write to one worktree.", ["10.2"], "M1b", M, ["T-19"], "Never allow multiple Implementers to write to the same worktree", None),
    ("R-INT-03", "Determine the change set, check it against the change scope before staging, and snapshot it as a candidate commit before any check runs.", ["10.2"], "M1b", M, ["T-06"], "Check the change set against the allowed change scope before staging anything", None),
    ("R-INT-04", "Create internal commits only on Bellona-managed refs, without running repository hooks and without requiring signing.", ["10.2"], "M1b", M, [], "without running repository hooks and without requiring commit signing", None),
    ("R-INT-05", "Reset the Phase worktree to the candidate commit before a repair Attempt continues in it.", ["10.2"], "M3", M, [], "reset the worktree to the candidate commit", None),
    ("R-INT-06", "Integrate a candidate by fast-forward when the integration ref still points at its base commit.", ["10.2"], "M1b", M, ["T-18"], "integrate by fast-forwarding to the candidate commit", None),
    ("R-INT-07", "Integrate one candidate at a time, and use a merge commit when the ref has moved; never resolve conflicts automatically.", ["10.2"], "M4", M, ["T-19"], "Perform one integration at a time", None),
    ("R-INT-08", "Run the mandatory checks on the integration candidate, reuse candidate evidence only for an identical tree, and do not advance the ref when verification fails.", ["10.2"], "M4", M, ["T-19"], "Do not advance the accepted integration ref if verification fails", None),
    ("R-INT-09", "Record the integration intent, update the ref by comparison with the expected old commit, persist the observed result, and commit `PhaseAccepted` only afterwards.", ["10.2"], "M1b", M, ["T-20"], "using a comparison with the expected old commit", None),
    ("R-INT-10", "Move the integration ref only forward within a Run (invariant 15).", ["10.2"], "M1b", M, ["T-19"], "The integration ref only moves forward within a Run", None),
    ("R-INT-11", "After a conflict or a post-integration failure, start the repair Attempt in a new worktree at the current integration commit, with the preserved candidate as a reference.", ["10.2"], "M4", M, [], "starts in a new worktree at the current integration commit", None),
    ("R-INT-12", "When a Run is `Completed`, report the final integration commit and the Bellona-managed ref that points at it.", ["10.2"], "M1b", M, [], "Bellona reports the final integration commit", None),
    # ---------------- Agent runtime (sections 5, 11, 11.1, 12.1, 14, 18)
    ("R-RUN-01", "Keep host-specific argument names and event formats out of the core.", ["11"], "M1a", M, [], "Keep host-specific argument names and event formats out of the core", None),
    ("R-RUN-02", "Take the agent's final message as its output; agents write no artifact files, and only the engine writes stored artifacts.", ["11"], "M1a", M, [], "The agent's final message is its output channel", None),
    ("R-RUN-03", "Derive the outcome of an execution from the host's result, never from the exit code alone (invariant 3).", ["11"], "M1a", M, [], "Derive the outcome from the host's result", None),
    ("R-RUN-04", "Treat every capability beyond the basic result as optional, and fail configuration validation at Run start when a role setting requires an absent capability.", ["11"], "M1a", M, [], "fails configuration validation at Run start instead of being silently ignored", None),
    ("R-RUN-05", "Store the launch receipt with the Attempt, and fail the Attempt when the receipt contradicts a value that the adapter set.", ["11"], "M1a", M, ["T-08"], "fails the Attempt as `MissingEnvironment`", None),
    ("R-RUN-06", "Record refused calls, and use them to classify a failure only when the Attempt produced no acceptable output.", ["11"], "M1a", M, ["T-08"], "Always record the calls that the host refused", None),
    ("R-RUN-07", "Build the environment of the agent process explicitly, and record variable names but never values.", ["11"], "M1a", M, ["T-08"], "The adapter builds the environment of the agent process explicitly", None),
    ("R-RUN-08", "Keep the role definition identical across the Attempts of a role, and put variable content in the user message.", ["11"], "M2", S_, [], "SHOULD be identical across the Attempts of a role", None),
    ("R-RUN-09", "Reject `args` that contain an adapter-owned argument, and pass the prompt separately from `args`.", ["14"], "M1a", M, ["T-08"], "configuration validation at Run start rejects `args` that contain one of them", None),
    ("R-RUN-10", "Assign the session ID before launch where the host supports it, and record it with the launch intent.", ["12.1"], "M1a", M, ["T-08"], "assigned by the engine before launch where the host supports it", None),
    ("R-RUN-11", "Resume only by explicit session ID, and pass the instructions of a resumed Turn in the user message.", ["11.1"], "M2", M, [], "Resume only by explicit session ID", None),
    ("R-RUN-12", "Bind a session to one role and one worktree, never run two Attempts in one session at once, and never let an Evaluator reuse an Implementer session.", ["5"], "M1b", M, ["T-18"], "An Evaluator must not reuse an Implementer session", None),
    ("R-RUN-13", "Start every Evaluator Attempt in a new session.", ["11.1"], "M3", M, [], "Every Evaluator Attempt starts in a new session", None),
    ("R-RUN-14", "Resume the Planner session for later Planner Attempts, after updating its worktree to the current integration commit and taking the comparison baseline.", ["5"], "M2", M, ["T-10"], "Before a resumed Planner Turn, update the Planner's worktree", None),
    ("R-RUN-15", "Disallow native subagent delegation for the Implementer and the Coordinator as a role contract, and record subagent use where the host reports it.", ["11.1"], "M1a", M, ["T-10"], "disallow native subagent delegation as a role contract", None),
    ("R-RUN-16", "If read-only subagents are enabled for the Planner or the Evaluator, treat them as part of the Attempt, and reject the setting for the Implementer and the Coordinator.", ["11.1"], PM, M, ["T-10"], "Configuration validation rejects `subagents: readOnly`", None),
    ("R-RUN-17", "Launch each role as an independent CLI process, with a new process for every Turn in the MVP.", ["11.1"], "M1a", M, [], "launches a new process for every Turn", None),
    ("R-RUN-18", "Probe the capabilities of each target host and answer its open questions before the AgentRuntime contract is frozen.", ["18"], "M0", M, [], "before the AgentRuntime contract is frozen", None),
    ("R-RUN-19", "Run the same workflow with a second host adapter, and report host-specific limitations.", ["18"], "M5", M, [], "Execute the same workflow with another harness", None),
    ("R-RUN-20", "Record cache usage for every Attempt.", ["11"], "M1a", M, [], "record cache usage for every Attempt", None),
    # ---------------- Process supervision (section 11.2)
    ("R-PROC-01", "Launch agents in dedicated process groups, reap them with `Wait`, and consume stdout and stderr concurrently.", ["11.2"], "M1a", M, ["T-15"], "Consume stdout and stderr concurrently", None),
    ("R-PROC-02", "Enforce the startup, Turn, Phase, and Run timeouts and the cancellation grace period.", ["11.2"], "M1a", M, ["T-05", "T-15"], "Cancellation grace period", None),
    ("R-PROC-03", "Measure active time as wall-clock time with at least one supervised process running, without adding parallel processes together.", ["11.2"], "M1a", M, [], "Time spent by processes running in parallel is not added together", None),
    ("R-PROC-04", "Cancel in stages: a graceful interrupt, termination of the process group, and forced termination after the grace period; apply the same stages when a Turn times out.", ["11.2"], "M1a", M, ["T-08", "T-15"], "Cancellation is staged", None),
    ("R-PROC-05", "End a cancelled or timed-out Attempt as `Cancelled` or `TimedOut`, and do not accept output produced during the graceful interrupt.", ["11.2"], "M1a", M, ["T-08"], "that output is kept for inspection and is not accepted", None),
    ("R-PROC-06", "Never identify a surviving process by PID alone, and do not terminate a process whose identity is uncertain.", ["11.2"], "M3", M, [], "Do not identify surviving processes by PID alone", None),
    ("R-PROC-07", "Detect idle agents from gaps in events, without treating silence during inference as a failure.", ["11.2"], PM, M, ["T-15"], "Idle timeout / heartbeat", None),
    # ---------------- Persistence (sections 12, 12.1, 12.2)
    ("R-PER-01", "Keep per-Run files under the XDG state directory, and introduce no external database.", ["12"], "M0", M, [], "Create per-Run files under XDG State Home", None),
    ("R-PER-02", "Treat the append-only `events.jsonl` as the authority for Run state, and `state.json` as a snapshot that can be rebuilt from it.", ["12"], "M0", M, ["T-04", "T-13"], "is the authoritative source for Run state", None),
    ("R-PER-03", "Prevent concurrent modification with a per-Run OS file lock, and use a single writer inside the engine.", ["12"], "M0", M, [], "with a per-Run OS file lock", None),
    ("R-PER-04", "Finalize an artifact by temporary file, sync, and atomic rename before appending the event that references it, and update the snapshot after the event is synchronized.", ["12"], "M0", M, ["T-04", "T-12"], "atomically renaming it into place", None),
    ("R-PER-05", "On resume, validate sequence numbers, formats, and artifact references; recover an incomplete trailing record, and never ignore corruption in the middle of the journal.", ["12"], "M0", M, ["T-04", "T-13"], "never silently ignore corruption in the middle of the journal", None),
    ("R-PER-06", "Keep replay free of side effects: the Reducer derives state only from committed event values (invariant 13).", ["12.1"], "M0", M, ["T-13"], "The Reducer does not access clocks", None),
    ("R-PER-07", "Put only small data and artifact references (path, size, SHA-256) in events; keep finalized artifacts immutable, and stop when one is lost or does not match.", ["12.2"], "M0", M, ["T-13"], "Finalized artifact files are immutable", None),
    ("R-PER-08", "Configure size limits for artifacts, events, output records, questions, and answers, and never truncate required input silently.", ["12.2"], "M1a", M, ["T-16"], "Do not silently truncate required input", None),
    ("R-PER-09", "Give each Attempt its own directory with its binding and per-Turn outputs; agents never write into the Run directory.", ["12"], "M1a", M, [], "Each Attempt has its own directory", None),
    ("R-PER-10", "Do not delete plans, events, or logs automatically; remove them only on an explicit user command.", ["12"], "M0", M, [], "does not delete plans, events, or logs automatically", None),
    # ---------------- Recovery (sections 12, 12.1)
    ("R-REC-01", "Persist the launch intent before launch, record the confirmed launch afterwards, and record observed completion as a separate event.", ["12"], "M1a", M, ["T-12"], "Persist execution intent (`AttemptScheduled`) before launch", None),
    ("R-REC-02", "On recovery, acquire the Run lock, replay events, and then reconcile external state as the reconciliation table prescribes.", ["12.1"], "M3", M, ["T-12"], "first acquire the Run lock, replay events, and then reconcile external state", None),
    ("R-REC-03", "Mark an Attempt whose outcome cannot be determined `Unknown`, enter `RecoveryRequired`, and neither rerun nor discard it automatically.", ["12.1"], "M3", M, ["T-12"], "Do not automatically rerun or discard", None),
    ("R-REC-04", "Do not rerun an accepted Phase while its evidence is valid; when the evidence is stale or corrupt, rerun the checks on the recorded commit.", ["12.1"], "M3", M, [], "Rerun the mandatory checks on the recorded commit", None),
    ("R-REC-05", "Create a new Attempt for every retry, and never overwrite an earlier Attempt.", ["12.1"], "M1a", M, [], "Create a new Attempt for each retry without overwriting previous Attempts", None),
    # ---------------- Context (section 12.3)
    ("R-CTX-01", "Store the workflow context in Bellona and leave the conversation context to the host; implement no conversation store or compaction.", ["12.3"], "M1a", M, [], "Bellona does not implement its own conversation store or context compaction", None),
    ("R-CTX-02", "Give each role the context that the role table requires, as a fixed input contract.", ["12.3"], "M1b", M, [], "The MVP uses fixed role-specific input contracts", None),
    ("R-CTX-03", "Do not share the Implementer's conversation with the Evaluator.", ["12.3"], "M3", M, ["T-16"], "Do not share the Implementer's entire conversation by default", None),
    ("R-CTX-04", "When a session is lost or the host cannot resume, continue in a new session from persisted artifacts and workspace state, and stop as `Blocked` only when a handoff input is missing.", ["12.3"], "M2", M, ["T-16"], "continue in a new session from persisted artifacts and workspace state", None),
    # ---------------- Run manifest (section 12.4)
    ("R-MAN-01", "Freeze the effective configuration, the role definitions, and the workflow version at Run start by copying their contents (invariant 12).", ["12.4"], "M0", M, ["T-14"], "hashes alone are insufficient", None),
    ("R-MAN-02", "Refuse to resume when the engine or the event contracts are incompatible, unless an explicit migration applies.", ["12.4"], "M0", M, ["T-14"], "otherwise refuse resumption", None),
    ("R-MAN-03", "Record Runtime updates and changes in model availability, and apply no configuration change to an existing Run except a journaled policy amendment.", ["12.4"], "M1a", M, ["T-14"], "do not silently apply configuration changes", None),
    ("R-MAN-04", "Keep the intended host customization scope and the launch receipt with the Run.", ["12.4"], "M1a", M, ["T-08"], "Keep both with the Run", None),
    # ---------------- Workspace (sections 4, 13)
    ("R-WS-01", "Record the repository state at Run start, reject a dirty working tree by default, and never overwrite uncommitted changes.", ["13"], "M1b", M, ["T-06"], "By default, the MVP rejects a dirty working tree", None),
    ("R-WS-02", "Create worktrees under `workspace.root`, never inside the user's working tree, and run every role in a Bellona-managed worktree.", ["13"], "M1b", M, ["T-06"], "Worktrees are never created inside the user's working tree", None),
    ("R-WS-03", "Run the `workspace.setup` commands in every new worktree before use, and classify a setup failure as `MissingEnvironment`.", ["13"], "M1b", M, ["T-06"], "A setup failure is `MissingEnvironment`", None),
    ("R-WS-04", "Compare the tracked files of the worktree before and after each execution of a read-only role, and do not accept the output when they changed.", ["4"], "M2", M, ["T-06"], "a change to them is a role contract violation", "Needed when the first read-only role, the Planner, arrives in M2."),
    ("R-WS-05", "Do not save credentials or API keys in artifacts or logs.", ["13"], "M1a", M, [], "Do not intentionally save credentials or API keys", None),
    ("R-WS-06", "Validate the paths, symbolic links, and sizes of artifacts that the engine loads.", ["13"], "M0", M, ["T-06"], "Validate paths, symbolic links, and sizes", None),
    ("R-WS-07", "Do not delete worktrees unconditionally at the end of a Run; check for unpreserved changes first.", ["13"], "M1b", M, ["T-06"], "Do not unconditionally delete worktrees", None),
    ("R-WS-08", "Never edit harness configuration files or add sandbox arguments, and add no permission arguments unless scope enforcement is enabled.", ["13"], "M1a", M, [], "never adds sandbox arguments", None),
    ("R-WS-09", "Use Git worktrees as the only workspace separation.", ["13"], "M1b", M, [], "Use only Git worktrees for workspace separation", None),
    ("R-WS-10", "Leave the host's per-directory session data in place when a worktree is removed, unless the user asks for its removal.", ["13"], "M1b", M, [], "leaves that data in place by default", None),
    # ---------------- Configuration (section 14)
    ("R-CFG-01", "Keep models, limits, and restrictions in user-managed JSON configuration, and never let an LLM or a lower-priority setting weaken restrictions or budget ceilings.", ["14"], "M0", M, [], "Do not grant LLMs authority to change configuration", None),
    ("R-CFG-02", "Define commands as named argument arrays, and resolve plan references, required checks, and workspace setup by name.", ["14"], "M1b", M, [], "`commands` defines named commands as argument arrays", None),
    ("R-CFG-03", "Apply the host customization scope of each role: `inherit` by default, and `isolated` by default for the Evaluator where the adapter supports it.", ["14"], "M1a", M, [], "The default is `inherit`, except for the Evaluator", None),
    ("R-CFG-04", "If scope enforcement is enabled, generate the role's permission rules from the Phase, own the permission arguments, record a fallback to detection, and keep the inspection after the Turn as the authority.", ["14"], PM, M, ["T-10"], "The engine's inspection after the Turn remains the authority", None),
    # ---------------- Errors and retries (section 14.1)
    ("R-ERR-01", "Classify errors as the error table prescribes, and keep separate persisted counters for transport retries, structured-output repairs, implementation repairs, and replanning.", ["14.1"], "M3", M, ["T-15"], "Persist separate counters", "Section 18 calls error classification and retry limits mandatory for the MVP and places error-specific recovery in M3."),
    ("R-ERR-02", "Retry a transient error only when the outcome of external side effects is certain, with capped backoff and jitter, and without stacking retries on those of the host.", ["14.1"], "M3", M, [], "use exponential backoff with a cap and jitter", None),
    ("R-ERR-03", "Treat a rate-limit rejection as a Run-wide event: stop new dispatches, suspend the Run as `Blocked` without waiting, and do not charge the rejected Attempt to a retry counter.", ["14.1"], "M4", M, [], "A rate-limit rejection is a Run-wide event", None),
    ("R-ERR-04", "Record the reason, the count, and the next eligible retry time of every retry, so that a restart cannot reset the limits.", ["14.1"], "M3", M, ["T-15"], "next eligible retry time", None),
    ("R-ERR-05", "End an Attempt that reaches a per-Turn cap as `Failed`, without stopping the Run.", ["14.1"], "M1a", M, ["T-08"], "This is not a Run-wide budget stop", None),
    ("R-ERR-06", "Preserve the worktree state on a scope violation, and do not snapshot or integrate it.", ["14.1"], "M1b", M, ["T-06"], "Preserve the worktree state and do not snapshot or integrate it", None),
    ("R-ERR-07", "Do not classify an unknown error as transient: treat it as `UnknownExecutionOutcome` after the launch intent was persisted, and stop as `Blocked` otherwise.", ["14.1"], "M3", M, [], "Do not classify an unknown error as transient", None),
    # ---------------- Budgets (section 14.2)
    ("R-BUD-01", "Manage Attempt counts, replan counts, and the Turn, Phase, and Run time limits.", ["14.2"], "M1a", M, ["T-01", "T-15"], "The MVP manages Attempt counts, replan counts", None),
    ("R-BUD-02", "Cap the agent steps and the estimated cost of each Turn where the host supports it, and record the caps as not applied where it does not.", ["14.2"], "M1a", M, ["T-08"], "record the caps as not applied", None),
    ("R-BUD-03", "Persist usage and cost only when the host reports them, never record an unavailable value as zero, and aggregate cumulative cost per session.", ["14.2"], "M1a", M, ["T-08"], "never as zero in aggregates", None),
    # ---------------- CLI and observability (sections 15, 15.1)
    ("R-CLI-01", "Provide `run`, `status`, `logs`, `inspect`, `resume`, and `cancel` as a foreground CLI with text output.", ["15"], "M1b", M, [], "use foreground execution and simple text output", None),
    ("R-CLI-02", "Provide `questions`, `answer`, and `approve`; show numbered choices with Other, and preserve questions on EOF.", ["15"], "M2", M, ["T-07"], "Preserve questions on EOF", None),
    ("R-CLI-03", "Record a policy amendment with `resume --amend` before resuming a `Blocked` Run.", ["15"], "M3", M, ["T-02"], "`resume --amend` records a policy amendment", None),
    ("R-CLI-04", "Keep the core engine independent of CLI input and output and of persistent execution modes, and require no daemon.", ["15.1"], "M0", M, [], "Keep the Core Engine independent of CLI input/output", None),
    ("R-CLI-05", "Distinguish domain events, agent events, process logs, decision records, and evidence, and correlate them by Run, Attempt, and session ID.", ["15.1"], "M1a", M, [], "Correlate them by Run ID, Attempt ID, and Session ID", None),
    ("R-CLI-06", "Keep confidential data out of logs where it would only be duplicated.", ["15.1"], "M1a", S_, [], "Logs SHOULD NOT duplicate confidential data", None),
    ("R-CLI-07", "Add `events` and `usage` commands when they are needed.", ["15.1"], PM, Y, [], "`events` and `usage` commands MAY be added", None),
    ("R-CLI-08", "Deliver `cancel` from another process through a cancellation request file that the control loop reads; for a suspended Run, record the cancellation under the Run lock.", ["15"], "M1b", M, [], "writes a cancellation request file into the Run directory", None),
    # ---------------- Technology and evaluation (sections 17, 18, 19)
    ("R-TECH-01", "Implement the core in Go with the standard library, and add no third-party Go module in the MVP unless a requirement cannot be met maintainably or correctly without one.", ["17"], "M0", M, [], "target zero third-party Go module dependencies in the MVP", None),
    ("R-TECH-02", "Test with the standard `testing` package, a fake runtime, and temporary Git repositories.", ["17"], "M0", M, [], "fake runtime, temporary Git repositories", None),
    ("R-TECH-03", "Begin the evaluation infrastructure in the first milestone.", ["18"], "M0", M, [], "Begin evaluation infrastructure", None),
    ("R-TECH-04", "Compare the variants of section 19 under the same task set, models, checks, and budgets, and do not use model self-evaluation alone as a success metric.", ["19"], "M4", M, [], "Do not use model self-evaluation alone as a success metric", "Section 19 does not say when the comparative evaluations run; placed at the end of the MVP."),
]

# Section 22 commitments -> rows that carry them (used only for the coverage check).
COMMITMENTS = {
    1: ["R-TECH-01"], 2: ["R-PER-01"], 3: ["R-FSM-03"], 4: ["R-EFF-01"], 5: ["R-WS-04", "R-RUN-12"],
    6: ["R-RUN-12"], 7: ["R-ART-01", "R-VER-01", "R-PER-02"], 8: ["R-SCH-03", "R-SCH-04", "R-INT-02", "R-INT-06", "R-INT-07"],
    9: ["R-WS-09", "R-WS-08"], 10: ["R-TECH-01", "R-CFG-01"], 11: ["R-HITL-09", "R-HITL-08"],
    12: ["R-PER-06", "R-REC-02"], 13: ["R-MAN-01", "R-FSM-15", "R-PER-07"], 14: ["R-CTX-01"],
    15: ["R-RUN-17"], 16: ["R-CLI-04"],
}

MILESTONES = ["M0", "M1a", "M1b", "M2", "M3", "M4", "M5", "M6", "M7", PM]
REAL_CLI_TEST = "T-11"  # tests with real CLI adapters; not counted as coverage
BEGIN = "## Appendix A. Requirements index"


def section_text(s, sec):
    if "." in sec:
        m = re.search(rf"(?m)^### {re.escape(sec)} .*$", s)
        if not m:
            return None
        nxt = re.search(r"(?m)^#{2,3} ", s[m.end():])
        return s[m.start(): m.end() + nxt.start()] if nxt else s[m.start():]
    m = re.search(rf"(?m)^## {sec}\. .*$", s)
    if not m:
        return None
    nxt = re.search(r"(?m)^## ", s[m.end():])
    chapter = s[m.start(): m.end() + nxt.start()] if nxt else s[m.start():]
    return re.split(r"(?m)^### \d+\.\d+ .*$", chapter)[0]


def number_tests(s):
    """Give every deterministic test item an identifier, keeping the identifiers that exist."""
    a = s.index("### Deterministic tests")
    b = s.index("### Comparative evaluations")
    lines = s[a:b].split("\n")
    numbered = re.compile(r"- \*\*T-(\d\d)\*\* ")
    existing = [int(m.group(1)) for m in (numbered.match(line) for line in lines) if m]
    dup = sorted({n for n in existing if existing.count(n) > 1})
    if dup:
        raise SystemExit(f"duplicate test identifiers in section 19: {dup}")
    nxt = max(existing, default=0) + 1
    out = []
    for line in lines:
        if line.startswith("- ") and not numbered.match(line):
            out.append(f"- **T-{nxt:02d}** " + line[2:])
            existing.append(nxt)
            nxt += 1
        else:
            out.append(line)
    intro = "Each item has an identifier so that Appendix A can refer to it."
    block = "\n".join(out)
    if intro not in block:
        block = block.replace("### Deterministic tests\n", "### Deterministic tests\n\n" + intro + "\n", 1)
    return s[:a] + block + s[b:], {f"T-{n:02d}" for n in existing}


def render():
    out = [BEGIN, ""]
    out.append("This index gives each requirement of the body an identifier, the milestone in which it first has to hold, a strength, and the deterministic tests of §19 that name it. It adds no rules (see Conventions). Identifiers are stable: do not renumber them when rows are added or removed.")
    out.append("")
    out.append("This appendix is generated by `docs/tools/gen_index.py`. To change it, edit the rows in that script and run it; do not edit the tables by hand.")
    out.append("")
    out.append("- **Milestone**: the milestones of §18, where M1 is divided into M1a and M1b. M0 to M4 form the MVP and correspond to P0 in §23. `Post-MVP` marks hardening items that §23 lists as P1 and optional features that have no milestone. M6 corresponds to P2, and M7 to P3.")
    out.append("- **Strength**: MUST, SHOULD, or MAY as defined in Conventions.")
    out.append(f"- **Tests**: the items of §19 that name the behaviour. A dash means that no deterministic test in §19 names it yet. {REAL_CLI_TEST}, the tests with real CLI adapters, is not counted as coverage.")
    out.append("- A requirement that begins with \"If … enabled\" applies only when that optional feature is offered.")
    out.append("")
    out.append("### Requirements by milestone")
    out.append("")
    out.append("| Milestone | Requirements |")
    out.append("| --- | --- |")
    for ms in MILESTONES:
        ids = [r[0] for r in ROWS if r[3] == ms]
        if ids:
            out.append(f"| {ms} | {', '.join(ids)} |")
    out.append("")
    for prefix, title in AREAS:
        rows = [r for r in ROWS if r[0].startswith(f"R-{prefix}-")]
        if not rows:
            continue
        out.append(f"### {title}")
        out.append("")
        out.append("| ID | Requirement | Section | Milestone | Strength | Tests |")
        out.append("| --- | --- | --- | --- | --- | --- |")
        for rid, text, secs, ms, strength, tests, _anchor, _note in rows:
            sec = ", ".join("§" + x for x in secs)
            out.append(f"| {rid} | {text} | {sec} | {ms} | {strength} | {', '.join(tests) if tests else '—'} |")
        out.append("")
    return "\n".join(out).rstrip() + "\n"


def check(s, test_ids):
    errs = []
    ids = [r[0] for r in ROWS]
    dup = {i for i in ids if ids.count(i) > 1}
    if dup:
        errs.append(f"duplicate ids: {sorted(dup)}")
    known_prefixes = {p for p, _ in AREAS}
    for rid, text, secs, ms, strength, tests, anchor, _ in ROWS:
        if rid.split("-")[1] not in known_prefixes:
            errs.append(f"{rid}: unknown area")
        if ms not in MILESTONES:
            errs.append(f"{rid}: bad milestone {ms}")
        if strength not in (M, S_, Y):
            errs.append(f"{rid}: bad strength {strength}")
        if "|" in text:
            errs.append(f"{rid}: pipe in text")
        for t in tests:
            if t not in test_ids or t == REAL_CLI_TEST:
                errs.append(f"{rid}: bad test {t}")
        texts = [section_text(s, x) for x in secs]
        if any(t is None for t in texts):
            errs.append(f"{rid}: section missing {secs}")
            continue
        if not any(anchor in t for t in texts):
            errs.append(f"{rid}: anchor not found in {secs}: {anchor!r}")
            continue
        t = next(t for t in texts if anchor in t)
        i = t.index(anchor)
        start = max(t.rfind(". ", 0, i), t.rfind("\n", 0, i), t.rfind("| ", 0, i)) + 1
        ends = [e for e in (t.find(". ", i + len(anchor)), t.find("\n", i + len(anchor))) if e != -1]
        sentence = t[start: min(ends) if ends else len(t)]
        has_should = bool(re.search(r"\bSHOULD\b", sentence))
        has_may = bool(re.search(r"\bMAY\b", sentence))
        if strength == S_ and not has_should:
            errs.append(f"{rid}: SHOULD row but the body sentence has no SHOULD: {sentence[:80]!r}")
        if strength == Y and not has_may:
            errs.append(f"{rid}: MAY row but the body sentence has no MAY: {sentence[:80]!r}")
        if strength == M and (has_should or has_may):
            errs.append(f"{rid}: MUST row but the body sentence uses SHOULD or MAY: {sentence[:80]!r}")
    # invariants 1..16
    covered = set()
    for r in ROWS:
        for m in re.finditer(r"invariants? ([0-9, and]+)\)", r[1]):
            covered |= {int(x) for x in re.findall(r"\d+", m.group(1))}
    missing = sorted(set(range(1, 17)) - covered)
    if missing:
        errs.append(f"invariants without a row: {missing}")
    for n, rids in COMMITMENTS.items():
        for rid in rids:
            if rid not in ids:
                errs.append(f"commitment {n}: unknown row {rid}")
    if set(COMMITMENTS) != set(range(1, 17)):
        errs.append("commitment mapping incomplete")
    used = {t for r in ROWS for t in r[5]}
    unused = sorted(test_ids - used - {REAL_CLI_TEST})
    return errs, unused


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    unknown = [a for a in sys.argv[1:] if a.startswith("--") and a != "--check-only"]
    if len(args) != 1 or unknown:
        raise SystemExit(__doc__)
    check_only = "--check-only" in sys.argv
    path = args[0]
    original = open(path, encoding="utf-8").read()
    s, test_ids = number_tests(original)
    if BEGIN in s:
        s = s[: s.index(BEGIN)].rstrip() + "\n"
    errs, unused = check(s, test_ids)
    for e in errs:
        print("ERROR", e)
    by = {ms: sum(1 for r in ROWS if r[3] == ms) for ms in MILESTONES}
    print(f"rows: {len(ROWS)}  errors: {len(errs)}  without a test: {sum(1 for r in ROWS if not r[5])}")
    print("by milestone:", ", ".join(f"{ms} {n}" for ms, n in by.items() if n))
    if unused:
        print("test items that no row refers to:", ", ".join(unused))
    if errs:
        sys.exit(1)
    pointer = "Appendix A assigns each requirement to one of these milestones."
    if pointer not in s:
        s = s.replace("The first practical MVP covers M0–M4.", pointer + " The first practical MVP covers M0–M4.", 1)
    s = s.rstrip() + "\n\n" + render()
    if check_only:
        if s != original:
            print("out of date: run without --check-only to regenerate Appendix A")
            sys.exit(1)
        print("up to date")
    elif s != original:
        open(path, "w", encoding="utf-8").write(s)
        print("written")
    else:
        print("unchanged")


if __name__ == "__main__":
    main()
