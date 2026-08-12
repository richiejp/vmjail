# Source-blind product testing

The tester sees this directory, an installed `daft` CLI, public usage
documentation, fixture projects, and optionally the trusted MCP server. It
must not receive the darkaifactory source checkout or implementation logs.

Explore public workflows as a user would. A suspected problem is valid only
after two clean reproductions. Classify it as `correctness`, `ux`, or
`performance`, save each scenario run's JSON output below `runs/`, and append
one JSON object to `issues.jsonl`. Do not infer implementation causes.

Run a scenario:

```sh
daft blackbox scenario --file scenarios/example.json > runs/example-1.json
```

The command returns non-zero when an assertion fails but still prints complete
evidence. Repeat from clean state and retain both outputs.

Name each scenario file `<scenario.name>.json`. An issue must reference two
distinct failed run records for that exact scenario.

Validate the issue ledger:

```sh
daft blackbox issues --file issues.jsonl
```

`history.jsonl` is the tracker-independent, source-blind regression context.
It may be assembled from any issue tracker. Records contain only public
symptoms and expectations; `sources` retain opaque provenance without copying
diagnoses, source paths, host details, credentials, or private logs. Validate
it with:

```sh
daft blackbox history --file history.jsonl
```

History format:

```json
{"version":1,"id":"history-example","category":"correctness","title":"Prior failure","status":"resolved","symptom":"A public command returned stale data.","expected":"The command returns current data.","public_surfaces":["workspace status"],"sources":[{"system":"tracker","reference":"issue-123"}]}
```

Scenario format:

```json
{
  "version": 1,
  "name": "public-help-smoke",
  "steps": [{
    "name": "help",
    "argv": ["daft", "--help"],
    "timeout_seconds": 5,
    "want_exit": 0,
    "stdout_contains": ["darkaifactory CLI"]
  }]
}
```

Issue format (one compact object per line):

```json
{"id":"bb-example","category":"ux","title":"Example","status":"open","scenario":"public-help-smoke","expected":"Clear result","observed":"Ambiguous result","reproduced":2,"evidence":["runs/example-1.json","runs/example-2.json"],"first_seen_version":"0.1.0"}
```

Finite commands require tight measured deadlines. Performance findings must
state a repeatable budget in the scenario. Never write credentials, absolute
host paths, source paths, or unbounded command output into evidence.
