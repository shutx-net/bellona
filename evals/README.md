# Evaluations

This directory holds the evaluation tasks of the comparative evaluations of `SPEC.md` §19. M0 begins the
infrastructure (R-TECH-03): the task format, the result record format and two sample tasks. No trial has been
run and no result is recorded. Running the variants against the task set and comparing them starts with
R-TECH-04 in M4.

## Layout

```text
evals/
├── README.md
└── tasks/
    └── <id>/                  the directory name is the task id
        ├── task.json          the task
        └── testdata/
            ├── repo/          the initial repository (becomes the initial commit)
            └── acceptance/    hidden acceptance tests (copied over the repository after the run)
```

The fixtures live under `testdata` so that the Go tool ignores them. They must still be `gofmt`-clean, because the
formatting check scans them. The fixture modules declare `go 1.22` so that they also build with older toolchains
in evaluation environments.

## Task format

`task.json` is a strict JSON contract: unknown members and `null` are rejected, and a member is optional only
where it can be omitted. The Go types in `internal/evals` (`Task`, `LoadTask`) are authoritative. The members are
`schemaVersion`, `id`, `title`, `repository`, `request`, `setup`, `acceptance`, `expectedChangeScope` and `tags`.

- `repository` is either `{"kind": "directory", "path": ...}` (a task-relative directory whose content becomes the
  initial commit) or `{"kind": "git", "url": ..., "commit": ...}` (a remote repository at a 40-character lowercase
  hexadecimal commit).
- Commands (`setup`, `acceptance.checks`, `acceptance.regressionChecks`) are `{"id", "argv", "timeout"}` with an
  argument array, never a shell string, and an optional positive Go duration.
- `acceptance.files` is a task-relative directory of hidden acceptance tests. `acceptance.checks` must all exit 0
  for a trial to be accepted. `acceptance.regressionChecks` pass on the initial repository, and one that fails
  after the run counts as a regression.
- `expectedChangeScope` uses the change-scope grammar of `SPEC.md` §8. A file that a trial changes outside it counts
  as an unnecessary change.

`LoadTask` also requires the task id to equal the directory name, and the repository directory and the acceptance
directory to exist inside the task directory without a symbolic link on the way or below. A test keeps the
acceptance files out of the repository, and checks that every sample task starts unsolved: its regression checks
pass on a temporary Git repository made from the initial repository, and after the acceptance files are copied in,
at least one acceptance check fails.

## Result record

A trial produces one result record. The Go types `Result`, `Variant`, `RoleModel`, `CheckSummary`,
`CheckOutcome` and `Tokens` in `internal/evals` are authoritative. A record carries the task, the variant, the
trial number, the fixed effort (Baseline B only), the Bellona version (absent for Baseline A), the requested and
confirmed model of each role, the start and finish time in UTC, the outcome of each acceptance check, the number of
regressions, the number of unnecessary changed files, the tokens, the API-equivalent and the billed cost, the
retry count and the plan revision count.

- A value that was not measured is an absent member, never zero. A recorded zero is a measurement.
- Token use and cost as a multiple of Baseline A are computed when results are reported and are not stored.
- A model's evaluation of its own work is never a success metric (R-TECH-04), so the record has no field for it.

## Procedure of one trial

1. Prepare the repository as a fresh Git repository from the task's initial repository.
2. Run `setup`.
3. Give the request to the variant.
4. Run the regression checks.
5. Copy the acceptance files over the repository.
6. Run the acceptance checks.
7. Count the changed files that lie outside `expectedChangeScope`.
8. Record a result.

## Variants

Every variant works on the same task set, with the same models, verification conditions and initial code, and a
comparable budget, over multiple trials.

| Variant | Description | Settings |
| --- | --- | --- |
| `baseline-a` | Single agent plus the mandatory checks | Runs outside Bellona with the same checks, until the Direct strategy exists |
| `baseline-b` | Fixed-effort Planner and Implementer | `independentReview: none`; run once at each permitted effort level, so that adaptive effort is compared with the best fixed level |
| `variant-c` | Adaptive-effort Planner and Implementer | `independentReview: none` |
| `variant-d` | Adaptive effort and an independent Evaluator | |
| `variant-e` | Variant D with session reuse and parallel scheduling | |

The calibration of the Evaluator (its miss rate on seeded defects and its false-rejection rate on correct diffs) is
separate from these trials and is not covered by these formats yet.
