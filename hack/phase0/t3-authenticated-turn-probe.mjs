import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdtemp, mkdir, rm } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { connectT3Rpc } from "./t3-rpc-client.mjs";

const runFile = promisify(execFile);
const t3Binary = process.env.T3_PHASE0_T3_BIN;
const mutationConsent = process.env.T3_PHASE0_ALLOW_MUTATION;
const modelUsageConsent = process.env.T3_PHASE0_ALLOW_MODEL_USAGE;
const codexBinary = process.env.T3_PHASE0_CODEX_BIN ?? "codex";
const claudeBinary = process.env.T3_PHASE0_CLAUDE_BIN ?? "claude";
const preferredModels = {
  codex: "gpt-5.6-luna",
  claudeAgent: "claude-haiku-4-5",
};

function after(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function makeId(kind) {
  return `phase0:${kind}:${randomUUID()}`;
}

function authorizationHeaders(token, hasBody = false) {
  return {
    Authorization: `Bearer ${token}`,
    ...(hasBody ? { "Content-Type": "application/json" } : {}),
  };
}

async function reservePort() {
  const server = createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  assert.notEqual(address, null);
  assert.equal(typeof address, "object");
  const port = address.port;
  await new Promise((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
  return port;
}

function startT3Server(port, baseDirectory, workspaceRoot) {
  const child = spawn(
    t3Binary,
    [
      "start",
      "--mode",
      "web",
      "--host",
      "127.0.0.1",
      "--port",
      String(port),
      "--base-dir",
      baseDirectory,
      "--no-browser",
      workspaceRoot,
    ],
    {
      env: { ...process.env, NO_COLOR: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  child.on("error", () => {});
  child.stdout.resume();
  child.stderr.resume();
  return child;
}

async function waitForServer(child, httpBaseUrl) {
  const deadline = Date.now() + 90_000;
  do {
    if (child.exitCode !== null) {
      throw new Error("isolated t3 server exited before it became ready");
    }
    try {
      const response = await fetch(
        new URL("/.well-known/t3/environment", httpBaseUrl),
        { signal: AbortSignal.timeout(2_000) },
      );
      if (response.ok) {
        return;
      }
    } catch {}
    await after(250);
  } while (Date.now() < deadline);
  throw new Error("isolated t3 server did not become ready");
}

async function waitForAuthenticatedSession(httpBaseUrl, bearerToken) {
  const deadline = Date.now() + 60_000;
  do {
    try {
      const response = await fetch(new URL("/api/auth/session", httpBaseUrl), {
        headers: authorizationHeaders(bearerToken),
        signal: AbortSignal.timeout(5_000),
      });
      if (response.ok && (await response.json()).authenticated === true) {
        return;
      }
    } catch {}
    await after(250);
  } while (Date.now() < deadline);
  throw new Error("isolated t3 authentication did not become ready");
}

async function connectWhenReady(httpBaseUrl, bearerToken) {
  const deadline = Date.now() + 60_000;
  do {
    try {
      return await connectT3Rpc({
        httpBaseUrl,
        bearerToken,
        connectTimeoutMs: 5_000,
      });
    } catch {
      await after(250);
    }
  } while (Date.now() < deadline);
  throw new Error("isolated t3 RPC did not become ready");
}

async function stopT3Server(child, requireGraceful) {
  if (!child || child.exitCode !== null) {
    return 0;
  }
  const startedAt = Date.now();
  const exitPromise = new Promise((resolve) => child.once("exit", resolve));
  child.kill("SIGTERM");
  const stopped = await Promise.race([
    exitPromise.then(() => true),
    after(20_000).then(() => false),
  ]);
  if (!stopped) {
    child.kill("SIGKILL");
    await exitPromise;
    if (requireGraceful) {
      throw new Error("idle t3 server required SIGKILL");
    }
  }
  return Date.now() - startedAt;
}

async function issueAdminSession(baseDirectory) {
  const result = await runFile(
    t3Binary,
    [
      "auth",
      "session",
      "issue",
      "--base-dir",
      baseDirectory,
      "--ttl",
      "30m",
      "--label",
      "phase0-authenticated-turn",
      "--subject",
      "phase0-authenticated-turn",
      "--json",
    ],
    { killSignal: "SIGKILL", maxBuffer: 1024 * 1024, timeout: 30_000 },
  );
  return JSON.parse(result.stdout);
}

async function revokeAdminSession(baseDirectory, sessionId) {
  await runFile(
    t3Binary,
    ["auth", "session", "revoke", "--base-dir", baseDirectory, sessionId],
    { killSignal: "SIGKILL", timeout: 30_000 },
  );
}

async function fetchJson(httpBaseUrl, bearerToken, path, options = {}) {
  const hasBody = options.body !== undefined;
  const response = await fetch(new URL(path, httpBaseUrl), {
    ...options,
    headers: {
      ...authorizationHeaders(bearerToken, hasBody),
      ...options.headers,
    },
    signal: AbortSignal.timeout(options.timeoutMs ?? 15_000),
  });
  if (!response.ok) {
    throw new Error(`${path} failed with ${response.status}`);
  }
  return response.json();
}

async function assertHttpBootstrapBug(httpBaseUrl, bearerToken, command) {
  const response = await fetch(
    new URL("/api/orchestration/dispatch", httpBaseUrl),
    {
      method: "POST",
      headers: authorizationHeaders(bearerToken, true),
      body: JSON.stringify(command),
      signal: AbortSignal.timeout(30_000),
    },
  );
  assert.equal(response.status, 500);
}

async function dispatch(rpc, command) {
  return rpc.request("orchestration.dispatchCommand", command, {
    timeoutMs: 30_000,
  });
}

async function getShell(httpBaseUrl, bearerToken) {
  return fetchJson(httpBaseUrl, bearerToken, "/api/orchestration/shell");
}

async function getThread(httpBaseUrl, bearerToken, threadId) {
  return fetchJson(
    httpBaseUrl,
    bearerToken,
    `/api/orchestration/threads/${encodeURIComponent(threadId)}`,
  );
}

async function waitForProvider(rpc, instanceId) {
  const deadline = Date.now() + 120_000;
  let observed;
  do {
    const refreshed = await rpc.request(
      "server.refreshProviders",
      { instanceId },
      { timeoutMs: 60_000 },
    );
    observed = refreshed.providers.find(
      (provider) => provider.instanceId === instanceId,
    );
    if (
      observed?.installed === true &&
      observed.enabled === true &&
      observed.availability !== "unavailable"
    ) {
      return observed;
    }
    await after(250);
  } while (Date.now() < deadline);
  throw new Error(`provider ${instanceId} did not become available`);
}

function selectModel(provider, driver) {
  const preferred = provider.models.find(
    (model) => model.slug === preferredModels[driver],
  );
  const selected =
    preferred ??
    provider.models.find((model) => model.isDefault === true) ??
    provider.models.find((model) => model.isLegacy !== true) ??
    provider.models[0];
  return selected?.slug ?? preferredModels[driver];
}

function threadIsActive(thread) {
  return (
    thread.latestTurn?.state === "running" ||
    thread.session?.status === "starting" ||
    thread.session?.status === "running" ||
    (thread.session?.activeTurnId ?? null) !== null ||
    thread.hasPendingApprovals === true ||
    thread.hasPendingUserInput === true ||
    thread.backgroundLiveness === "working" ||
    thread.backgroundLiveness === "monitoring"
  );
}

async function waitForTurn({
  httpBaseUrl,
  bearerToken,
  threadId,
  expectedText,
  requireActiveObservation,
}) {
  const deadline = Date.now() + 360_000;
  let activeObserved = false;
  do {
    const shell = await getShell(httpBaseUrl, bearerToken);
    const thread = shell.threads.find((candidate) => candidate.id === threadId);
    if (thread) {
      activeObserved ||= threadIsActive(thread);
      if (thread.latestTurn?.state === "error") {
        throw new Error(`provider turn failed for ${threadId}`);
      }
      if (thread.latestTurn?.state === "completed" && !threadIsActive(thread)) {
        const detail = await getThread(httpBaseUrl, bearerToken, threadId);
        const assistantMessage = detail.thread.messages.find(
          (message) =>
            message.role === "assistant" &&
            message.turnId === thread.latestTurn.turnId &&
            message.streaming === false,
        );
        assert.ok(assistantMessage, `assistant response missing for ${threadId}`);
        assert.notEqual(assistantMessage.text.trim().length, 0);
        if (
          expectedText !== undefined &&
          !assistantMessage.text.includes(expectedText)
        ) {
          throw new Error(
            `assistant response did not contain the continuity marker for ${threadId}`,
          );
        }
        if (requireActiveObservation) {
          assert.equal(activeObserved, true, `active state was not observed for ${threadId}`);
        }
        return { activeObserved, turnId: thread.latestTurn.turnId };
      }
    }
    await after(100);
  } while (Date.now() < deadline);
  throw new Error(`provider turn did not complete for ${threadId}`);
}

function turnCommand({ threadId, messageText, modelSelection, bootstrap }) {
  return {
    type: "thread.turn.start",
    commandId: makeId("command"),
    threadId,
    message: {
      messageId: makeId("message"),
      role: "user",
      text: messageText,
      attachments: [],
    },
    ...(modelSelection ? { modelSelection } : {}),
    runtimeMode: "full-access",
    interactionMode: "default",
    ...(bootstrap ? { bootstrap } : {}),
    createdAt: new Date().toISOString(),
  };
}

async function assertEveryThreadIdle(httpBaseUrl, bearerToken, threadIds) {
  const shell = await getShell(httpBaseUrl, bearerToken);
  for (const threadId of threadIds) {
    const thread = shell.threads.find((candidate) => candidate.id === threadId);
    assert.ok(thread, `thread ${threadId} missing from shell snapshot`);
    assert.equal(threadIsActive(thread), false, `thread ${threadId} was not idle`);
    assert.equal(thread.latestTurn?.state, "completed");
    assert.equal(thread.session?.status, "ready");
    assert.equal(thread.session?.activeTurnId, null);
    assert.equal(thread.session?.lastError, null);
  }
}

async function waitForActiveThread(httpBaseUrl, bearerToken, threadId) {
  const deadline = Date.now() + 120_000;
  do {
    const shell = await getShell(httpBaseUrl, bearerToken);
    const thread = shell.threads.find((candidate) => candidate.id === threadId);
    if (thread && threadIsActive(thread) && thread.latestTurn?.state === "running") {
      return thread.latestTurn.turnId;
    }
    await after(50);
  } while (Date.now() < deadline);
  throw new Error(`active state was not observed for ${threadId}`);
}

async function observeRestartedTurn(
  httpBaseUrl,
  bearerToken,
  threadId,
  turnId,
) {
  const deadline = Date.now() + 60_000;
  let observed;
  do {
    const shell = await getShell(httpBaseUrl, bearerToken);
    observed = shell.threads.find((candidate) => candidate.id === threadId);
    if (
      observed?.latestTurn?.turnId === turnId &&
      observed.latestTurn.state !== "running" &&
      !threadIsActive(observed)
    ) {
      return {
        active: false,
        latestTurnState: observed.latestTurn.state,
        sessionStatus: observed.session?.status ?? null,
      };
    }
    await after(100);
  } while (Date.now() < deadline);
  return {
    active: observed ? threadIsActive(observed) : null,
    latestTurnState: observed?.latestTurn?.state ?? null,
    sessionStatus: observed?.session?.status ?? null,
  };
}

if (!t3Binary) {
  throw new Error("T3_PHASE0_T3_BIN is required");
}

if (mutationConsent !== "isolated") {
  throw new Error("Set T3_PHASE0_ALLOW_MUTATION=isolated for an isolated server");
}

if (modelUsageConsent !== "authenticated") {
  throw new Error(
    "Set T3_PHASE0_ALLOW_MODEL_USAGE=authenticated to permit authenticated provider turns",
  );
}

const suffix = randomUUID().replaceAll("-", "").slice(0, 12);
const baseDirectory = await mkdtemp(join(tmpdir(), "t3-phase0-turn-base-"));
const workspaceRoot = await mkdtemp(join(tmpdir(), "t3-phase0-turn-workspace-"));
await mkdir(workspaceRoot, { recursive: true, mode: 0o700 });
const port = await reservePort();
const httpBaseUrl = `http://127.0.0.1:${port}`;
let projectId;
const definitions = [
  { driver: "codex", binaryPath: codexBinary },
  { driver: "claudeAgent", binaryPath: claudeBinary },
].map((definition) => ({
  ...definition,
  instanceId: `phase0_${suffix}_${definition.driver.toLowerCase()}`,
  threadId: `phase0_thread_${suffix}_${definition.driver.toLowerCase()}`,
  continuityValue: String(
    30_000 + (Number.parseInt(randomUUID().slice(0, 8), 16) % 2_768),
  ),
}));
const providerInstances = Object.fromEntries(
  definitions.map((definition) => [
    definition.instanceId,
    {
      driver: definition.driver,
      displayName: `Phase 0 ${definition.driver}`,
      enabled: true,
      environment: [],
      config: { binaryPath: definition.binaryPath },
    },
  ]),
);

let serverProcess;
let rpc;
let adminSession;
let idleShutdownMilliseconds;
let activeShutdownMilliseconds;
let activeShutdownOutcome;

try {
  adminSession = await issueAdminSession(baseDirectory);
  serverProcess = startT3Server(port, baseDirectory, workspaceRoot);
  await waitForServer(serverProcess, httpBaseUrl);
  await waitForAuthenticatedSession(httpBaseUrl, adminSession.token);
  rpc = await connectWhenReady(httpBaseUrl, adminSession.token);

  await rpc.request(
    "server.updateSettings",
    {
      patch: {
        enableProviderUpdateChecks: false,
        providerInstances,
      },
    },
    { timeoutMs: 60_000 },
  );

  for (const definition of definitions) {
    const provider = await waitForProvider(rpc, definition.instanceId);
    definition.modelSelection = {
      instanceId: definition.instanceId,
      model: selectModel(provider, definition.driver),
    };
  }

  const initialShell = await getShell(httpBaseUrl, adminSession.token);
  const registeredProject = initialShell.projects.find(
    (project) => project.workspaceRoot === workspaceRoot,
  );
  assert.ok(registeredProject, "t3 did not register its startup workspace");
  projectId = registeredProject.id;

  const httpBugDefinition = definitions[0];
  assert.ok(httpBugDefinition);
  await assertHttpBootstrapBug(
    httpBaseUrl,
    adminSession.token,
    turnCommand({
      threadId: `phase0_http_bug_${suffix}`,
      messageText: "This turn must not reach a provider.",
      modelSelection: httpBugDefinition.modelSelection,
      bootstrap: {
        createThread: {
          projectId,
          title: "Phase 0 HTTP bootstrap bug",
          modelSelection: httpBugDefinition.modelSelection,
          runtimeMode: "full-access",
          interactionMode: "default",
          branch: null,
          worktreePath: null,
          createdAt: new Date().toISOString(),
        },
      },
    }),
  );

  for (const definition of definitions) {
    await dispatch(
      rpc,
      turnCommand({
        threadId: definition.threadId,
        messageText: `In Kubernetes, we plan to expose a test Service on nodePort ${definition.continuityValue}. Which nodePort did I specify, and is it within the default NodePort range?`,
        modelSelection: definition.modelSelection,
        bootstrap: {
          createThread: {
            projectId,
            title: `Phase 0 ${definition.driver}`,
            modelSelection: definition.modelSelection,
            runtimeMode: "full-access",
            interactionMode: "default",
            branch: null,
            worktreePath: null,
            createdAt: new Date().toISOString(),
          },
        },
      }),
    );
    await waitForTurn({
      httpBaseUrl,
      bearerToken: adminSession.token,
      threadId: definition.threadId,
      expectedText: undefined,
      requireActiveObservation: false,
    });
  }

  await assertEveryThreadIdle(
    httpBaseUrl,
    adminSession.token,
    definitions.map((definition) => definition.threadId),
  );
  rpc.close();
  rpc = undefined;
  idleShutdownMilliseconds = await stopT3Server(serverProcess, true);
  serverProcess = undefined;

  serverProcess = startT3Server(port, baseDirectory, workspaceRoot);
  await waitForServer(serverProcess, httpBaseUrl);
  await waitForAuthenticatedSession(httpBaseUrl, adminSession.token);
  rpc = await connectWhenReady(httpBaseUrl, adminSession.token);
  for (const definition of definitions) {
    await waitForProvider(rpc, definition.instanceId);
  }

  for (const definition of definitions) {
    await dispatch(
      rpc,
      turnCommand({
        threadId: definition.threadId,
        messageText: "Which NodePort value did I specify earlier in this conversation?",
      }),
    );
    await waitForTurn({
      httpBaseUrl,
      bearerToken: adminSession.token,
      threadId: definition.threadId,
      expectedText: definition.continuityValue,
      requireActiveObservation: false,
    });
  }

  const codexDefinition = definitions.find(
    (definition) => definition.driver === "codex",
  );
  assert.ok(codexDefinition);
  await dispatch(
    rpc,
    turnCommand({
      threadId: codexDefinition.threadId,
      messageText: "Run a shell command that sleeps for 8 seconds to simulate a deployment readiness delay. When it ends, tell me the readiness check completed. Do not change files.",
    }),
  );
  await waitForTurn({
    httpBaseUrl,
    bearerToken: adminSession.token,
    threadId: codexDefinition.threadId,
    expectedText: undefined,
    requireActiveObservation: true,
  });

  await assertEveryThreadIdle(
    httpBaseUrl,
    adminSession.token,
    definitions.map((definition) => definition.threadId),
  );

  await dispatch(
    rpc,
    turnCommand({
      threadId: codexDefinition.threadId,
      messageText: "Run a shell command that sleeps for 12 seconds to simulate work during a planned restart. When it ends, report that the work completed.",
    }),
  );
  const interruptedTurnId = await waitForActiveThread(
    httpBaseUrl,
    adminSession.token,
    codexDefinition.threadId,
  );
  rpc.close();
  rpc = undefined;
  activeShutdownMilliseconds = await stopT3Server(serverProcess, true);
  serverProcess = undefined;

  serverProcess = startT3Server(port, baseDirectory, workspaceRoot);
  await waitForServer(serverProcess, httpBaseUrl);
  await waitForAuthenticatedSession(httpBaseUrl, adminSession.token);
  rpc = await connectWhenReady(httpBaseUrl, adminSession.token);
  activeShutdownOutcome = await observeRestartedTurn(
    httpBaseUrl,
    adminSession.token,
    codexDefinition.threadId,
    interruptedTurnId,
  );
} finally {
  rpc?.close();
  await stopT3Server(serverProcess, false);
  if (adminSession) {
    await revokeAdminSession(baseDirectory, adminSession.sessionId).catch(() => {});
  }
  await Promise.all([
    rm(baseDirectory, {
      recursive: true,
      force: true,
      maxRetries: 100,
      retryDelay: 100,
    }),
    rm(workspaceRoot, {
      recursive: true,
      force: true,
      maxRetries: 100,
      retryDelay: 100,
    }),
  ]);
}

process.stdout.write(
  `verified authenticated Codex and Claude conversation recovery across an idle t3 restart; idle SIGTERM completed in ${idleShutdownMilliseconds}ms\n`,
);
process.stdout.write(
  "verified the public shell snapshot reports active work during a Codex tool call and reports idle after completion\n",
);
process.stdout.write(
  "verified the pinned HTTP dispatch bootstrap bug remains; authenticated turns use the upstream WebSocket RPC path\n",
);
process.stdout.write(
  `observed active-turn SIGTERM exit in ${activeShutdownMilliseconds}ms with restarted state ${JSON.stringify(activeShutdownOutcome)}\n`,
);
