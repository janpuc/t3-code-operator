# Authenticated turn contract for pinned t3

Verified from the published `t3@0.0.34` source map and authenticated live
Codex and Claude turns on 2026-08-27.

## Source map references

All references below point into `dist/bin.mjs.map` in the pinned npm package.

| Ref | Source-map entry |
|---|---|
| S1 | `sources[111]`: `../../../packages/contracts/src/auth.ts` |
| S2 | `sources[157]`: `../../../packages/contracts/src/orchestration.ts` |
| S3 | `sources[164]`: `../../../packages/contracts/src/environmentHttp.ts` |
| S4 | `sources[183]`: `../../../packages/contracts/src/providerRuntime.ts` |
| S5 | `sources[199]`: `../../../packages/contracts/src/rpc.ts` |
| S6 | `sources[388]`: `../src/orchestration/Layers/ProjectionPipeline.ts` |
| S7 | `sources[396]`: `../src/orchestration/Layers/ProjectionSnapshotQuery.ts` |
| S8 | `sources[409]`: `../src/serverRuntimeStartup.ts` |
| S9 | `sources[546]`: `../src/ws.ts` |
| S10 | `sources[550]`: `../src/provider/Layers/ProviderSessionDirectory.ts` |
| S11 | `sources[559]`: `../src/provider/Layers/ProviderService.ts` |
| S12 | `sources[598]`: `../src/provider/Layers/ClaudeAdapter.ts` |
| S13 | `sources[617]`: `../src/provider/Layers/CodexSessionRuntime.ts` |
| S14 | `sources[705]`: `../src/orchestration/Layers/ProviderRuntimeIngestion.ts` |
| S15 | `sources[706]`: `../src/orchestration/Layers/ProviderCommandReactor.ts` |
| S16 | `sources[722]`: `../src/orchestration/http.ts` |

The source-map paths are authoritative for this pinned artifact. Line numbers
can change when a different npm release is pinned.

## Verified public seam

Use a bearer session with both `orchestration:read` and
`orchestration:operate`. Reads require the first scope. Dispatch requires the
second scope. [S1, S16]

The HTTP surface supports public snapshots and explicit commands:

| Method | Route | Result |
|---|---|---|
| `GET` | `/api/orchestration/shell` | `OrchestrationShellSnapshot` |
| `GET` | `/api/orchestration/threads/:threadId` | `OrchestrationThreadDetailSnapshot` |
| `POST` | `/api/orchestration/dispatch` | `{ "sequence": number }` |

Each request uses `Authorization: Bearer <token>`. The dispatch body is a
`ClientOrchestrationCommand`. [S2, S3]

The equivalent WebSocket methods are:

- `orchestration.dispatchCommand`
- `orchestration.subscribeShell`
- `orchestration.subscribeThread`

The existing RPC client obtains a one-use ticket from
`POST /api/auth/websocket-ticket`, then connects to `/ws?wsTicket=<ticket>`.
`orchestration.dispatchCommand` is the canonical path for a bootstrap turn.
`orchestration.subscribeThread` accepts `threadId`, optional `afterSequence`,
optional `requestCompletionMarker`, and optional `turnLimit`. It emits
`snapshot`, `event`, and `synchronized` items. `synchronized` means that stream
catch-up is complete. It does not mean that a model turn is complete. [S2, S5]

## Authenticated Codex or Claude turn

The public orchestration sequence is the same for both drivers. Only the
configured provider `instanceId` and model differ.

The pinned default model is `gpt-5.6-sol` for Codex and `claude-sonnet-5` for
Claude. A live probe must use a model available to the authenticated account.
It must not print the provider inventory. [S2]

### 1. Create or select a project

An existing project can be selected from `/api/orchestration/shell`. A new
project uses this dispatch body:

```json
{
  "type": "project.create",
  "commandId": "<uuid>",
  "projectId": "<uuid>",
  "title": "Phase 0 authenticated turn",
  "workspaceRoot": "<existing absolute directory>",
  "createWorkspaceRootIfMissing": false,
  "defaultModelSelection": {
    "instanceId": "<configured instance slug>",
    "model": "<available model slug>"
  },
  "createdAt": "<ISO-8601 time>"
}
```

### 2. Create one thread

```json
{
  "type": "thread.create",
  "commandId": "<uuid>",
  "threadId": "<uuid>",
  "projectId": "<project uuid>",
  "title": "Phase 0 authenticated turn",
  "modelSelection": {
    "instanceId": "<configured instance slug>",
    "model": "<available model slug>"
  },
  "runtimeMode": "full-access",
  "interactionMode": "default",
  "branch": null,
  "worktreePath": null,
  "createdAt": "<ISO-8601 time>"
}
```

### 3. Start one turn

```json
{
  "type": "thread.turn.start",
  "commandId": "<uuid>",
  "threadId": "<thread uuid>",
  "message": {
    "messageId": "<uuid>",
    "role": "user",
    "text": "<probe prompt>",
    "attachments": []
  },
  "modelSelection": {
    "instanceId": "<configured instance slug>",
    "model": "<available model slug>"
  },
  "runtimeMode": "full-access",
  "interactionMode": "default",
  "createdAt": "<ISO-8601 time>"
}
```

Each HTTP dispatch returns only an event-store `sequence`. It does not return
the provider turn ID. The probe must observe the turn ID from the shell or
thread state. [S2, S3]

The WebSocket handler supports `thread.turn.start.bootstrap.createThread`.
The HTTP handler sends the normalized command directly to the orchestration
engine. It does not run the WebSocket bootstrap program. A no-model live check
confirmed that the HTTP request returns status 500 because its thread does not
exist. This is a pinned upstream bug. The probe preserves it and uses the
WebSocket method that the upstream client uses. An HTTP client must send the
explicit `thread.create` command above. [S9, S16]

Send only one turn at a time. Claude treats another message during a real turn
as steering for that turn. Codex can queue a follow-up while a turn is active.
Single-turn serialization avoids an ambiguous completion target. [S12, S13]

## Observe the turn

Poll `/api/orchestration/shell`, or subscribe with
`orchestration.subscribeThread`. Record `session.activeTurnId` when it first
becomes non-null. [S2, S5]

The shell thread exposes:

- `latestTurn.turnId`
- `latestTurn.state`
- `latestTurn.requestedAt`
- `latestTurn.startedAt`
- `latestTurn.completedAt`
- `latestTurn.assistantMessageId`
- `session.status`
- `session.activeTurnId`
- `session.lastError`
- `hasPendingApprovals`
- `hasPendingUserInput`
- optional `backgroundLiveness`

`latestTurn.state` is `running`, `interrupted`, `completed`, or `error`.
Session status is `idle`, `starting`, `running`, `ready`, `interrupted`,
`stopped`, or `error`. [S2]

A successful target turn meets all of these conditions:

1. `latestTurn.turnId` equals the recorded target turn ID.
2. `latestTurn.state` is `completed`.
3. `latestTurn.completedAt` is non-null.
4. `session.status` is `ready`.
5. `session.activeTurnId` is null.
6. `session.lastError` is null.

The session leaving `running` is the authoritative turn-end signal. A
non-streaming assistant message does not prove completion while the session
still runs. `ready` or `idle` settles a running turn as completed. `error`
settles it as error. `stopped` or `interrupted` settles it as interrupted.
[S6]

A roll gate must also require both pending flags to be false. It must require
`backgroundLiveness` to be null if background tasks count as active work. A
terminal latest turn alone does not prove that background work has stopped.
This gate is a design inference from the public fields. [S2, S7]

The live probe observed `running` during an eight-second Codex shell command.
It then observed `latestTurn.state="completed"`, `session.status="ready"`, a
null `activeTurnId`, and no session error. Both Codex and Claude remained idle
while they waited for the next human message.

## Pending approval

`hasPendingApprovals=true` is the shell summary signal. Read the matching
thread detail and select the newest unresolved activity with
`kind="approval.requested"`. Its payload can contain:

- `requestId`
- `requestKind`
- `requestType`
- `detail`
- `appName`
- `options`

Respond through the same dispatch route:

```json
{
  "type": "thread.approval.respond",
  "commandId": "<uuid>",
  "threadId": "<thread uuid>",
  "requestId": "<request id from the activity>",
  "decision": "accept",
  "createdAt": "<ISO-8601 time>"
}
```

Allowed decisions are `accept`, `acceptForSession`, `acceptAlways`, `decline`,
and `cancel`. [S2, S4, S14]

The pending flag can clear when the response command enters the event store.
It can clear before the provider accepts the response. The roll gate must also
wait for the session and target turn to become terminal. [S6]

## Pending user input

`hasPendingUserInput=true` is the shell summary signal. Read the matching
thread detail and select the newest unresolved activity with
`kind="user-input.requested"`. Its payload contains `requestId` and
`questions`. Each question contains `id`, `header`, `question`, `options`, and
optional `multiSelect`. [S4, S14]

Respond through the same dispatch route:

```json
{
  "type": "thread.user-input.respond",
  "commandId": "<uuid>",
  "threadId": "<thread uuid>",
  "requestId": "<request id from the activity>",
  "answers": {
    "<question id>": "<selected answer>"
  },
  "createdAt": "<ISO-8601 time>"
}
```

The public answer type is a record of unknown values. The provider-specific
question contract determines whether one answer is a string or an array.
[S2, S4]

## Restart and conversation recovery

t3 does not expose a public `resumeSession` method. The next
`thread.turn.start` on the same `threadId` performs lazy internal recovery.
The persisted binding supplies the provider instance, working directory,
model selection, runtime mode, and opaque resume cursor. The public response
does not expose that cursor. [S10, S11, S15]

Codex persists `{ "threadId": "<provider thread id>" }` and asks the Codex
app server to resume that thread. Claude persists its session identifier and
passes it through the SDK `resume` option. [S12, S13]

A public recovery probe must use conversation content:

1. Complete a first turn that supplies a random valid NodePort value.
2. Stop t3 only after the safe roll predicate passes.
3. Restart t3 with the same t3 state directory and provider home.
4. Send a second turn on the same public `threadId`.
5. Ask for the NodePort without repeating it.
6. Require that value in the assistant response.

The state fields prove that t3 reopened the public thread. Only the remembered
value proves provider conversation recovery. The live probe passed this check
for both Codex and Claude. It used two model turns per driver.

An active turn does not survive a server restart as continuous work. Startup
marks an orphaned starting or running session as `error` and clears its active
turn. The error tells the user to send a new message. Therefore, the operator
must drain active work before it restarts t3. [S8]

The live probe signaled t3 during a separate Codex turn. T3 exited in 1.3
seconds. After restart, both the latest turn and the session had state `error`,
and the public active predicate was false. The pinned process does not let an
active turn finish on `SIGTERM`.

Pending approval and user-input callback state also does not survive restart
or recovered sessions. Upstream records the request as stale and tells the
user to restart the turn. The operator must not roll while either pending flag
is true. This is pinned upstream behavior, not an operator defect to repair.
[S15]

## Remaining live risks

- No public event distinguishes internal `adopt-existing` recovery from
  `resume-thread` recovery.
- Approval and user-input prompts are provider-directed. A deterministic
  black-box prompt still needs live validation.
- The public API has no quiesce gate. State observation cannot stop a new
  authorized client from dispatching work during a drain window.

MVP records and inherits these pinned upstream limits. It does not patch them.

## Repeatable authenticated probe

Run against the pinned installation:

```console
T3_PHASE0_T3_BIN=<path-to-pinned-t3> \
T3_PHASE0_ALLOW_MUTATION=isolated \
T3_PHASE0_ALLOW_MODEL_USAGE=authenticated \
node hack/phase0/t3-authenticated-turn-probe.mjs
```

Observed output:

```text
verified authenticated Codex and Claude conversation recovery across an idle t3 restart; idle SIGTERM completed in 1049ms
verified the public shell snapshot reports active work during a Codex tool call and reports idle after completion
verified the pinned HTTP dispatch bootstrap bug remains; authenticated turns use the upstream WebSocket RPC path
observed active-turn SIGTERM exit in 1307ms with restarted state {"active":false,"latestTurnState":"error","sessionStatus":"error"}
```

The probe refuses to run without both consent variables. It creates and removes
its t3 base directory and workspace. Authenticated provider tools can keep their
native session history in the existing Codex and Claude homes.
