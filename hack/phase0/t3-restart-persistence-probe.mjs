import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { access, chmod, mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { connectT3Rpc } from "./t3-rpc-client.mjs";

const runFile = promisify(execFile);
const t3Binary = process.env.T3_PHASE0_T3_BIN;
const mutationConsent = process.env.T3_PHASE0_ALLOW_MUTATION;
const codexBinary = process.env.T3_PHASE0_CODEX_BIN ?? "codex";
const claudeBinary = process.env.T3_PHASE0_CLAUDE_BIN ?? "claude";
const debugEnabled = process.env.T3_PHASE0_DEBUG === "1";

function debug(message) {
  if (debugEnabled) {
    process.stderr.write(`restart probe: ${message}\n`);
  }
}

function after(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

async function fileExists(path) {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
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

function captureWrapperSource(probe) {
  return [
    "#!/usr/bin/env node",
    'import { createHash } from "node:crypto";',
    'import { writeFileSync } from "node:fs";',
    'import { spawnSync } from "node:child_process";',
    `const value = process.env[${JSON.stringify(probe.environmentName)}] ?? "";`,
    'const digest = createHash("sha256").update(value).digest("hex");',
    `const homeMatches = process.env[${JSON.stringify(probe.homeEnvironmentName)}] === ${JSON.stringify(probe.homePath)};`,
    `const foreignValue = process.env[${JSON.stringify(probe.foreignEnvironmentName)}];`,
    `if (digest === ${JSON.stringify(sha256(probe.value))} && homeMatches && foreignValue === undefined) {`,
    `  writeFileSync(${JSON.stringify(probe.markerPath)}, "", { mode: 0o600 });`,
    "}",
    `const child = spawnSync(${JSON.stringify(probe.binaryPath)}, process.argv.slice(2), { stdio: "inherit" });`,
    "if (child.error) throw child.error;",
    "process.exit(child.status ?? 1);",
    "",
  ].join("\n");
}

async function writeCaptureWrapper(probe) {
  await writeFile(probe.wrapperPath, captureWrapperSource(probe), {
    mode: 0o700,
  });
  await chmod(probe.wrapperPath, 0o700);
}

function startT3Server(port, baseDirectory) {
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
      process.cwd(),
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
  const deadline = Date.now() + 60_000;
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
  let attempts = 0;
  do {
    attempts += 1;
    try {
      const response = await fetch(new URL("/api/auth/session", httpBaseUrl), {
        headers: { Authorization: `Bearer ${bearerToken}` },
        signal: AbortSignal.timeout(5_000),
      });
      if (response.ok && (await response.json()).authenticated === true) {
        debug(`authentication ready after ${attempts} attempt(s)`);
        return;
      }
    } catch {}
    await after(250);
  } while (Date.now() < deadline);
  throw new Error("isolated t3 authentication did not become ready");
}

async function connectWhenReady(httpBaseUrl, bearerToken) {
  const deadline = Date.now() + 60_000;
  let attempts = 0;
  do {
    attempts += 1;
    try {
      const connection = await connectT3Rpc({
        httpBaseUrl,
        bearerToken,
        connectTimeoutMs: 5_000,
      });
      debug(`RPC ready after ${attempts} attempt(s)`);
      return connection;
    } catch (error) {
      debug(`RPC attempt ${attempts} failed: ${error.message}`);
      await after(250);
    }
  } while (Date.now() < deadline);
  throw new Error("isolated t3 RPC did not become ready");
}

async function stopT3Server(child) {
  if (child.exitCode !== null) {
    return;
  }
  const exitPromise = new Promise((resolve) => child.once("exit", resolve));
  child.kill("SIGTERM");
  const stopped = await Promise.race([
    exitPromise.then(() => true),
    after(20_000).then(() => false),
  ]);
  if (stopped) {
    return;
  }
  child.kill("SIGKILL");
  await exitPromise;
  throw new Error("idle t3 server required SIGKILL during restart probe");
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
      "5m",
      "--label",
      "phase0-restart-persistence",
      "--subject",
      "phase0-restart-persistence",
      "--json",
    ],
    { killSignal: "SIGKILL", maxBuffer: 1024 * 1024, timeout: 30_000 },
  );
  return JSON.parse(result.stdout);
}

async function revokeAdminSession(baseDirectory, sessionId) {
  await runFile(
    t3Binary,
    [
      "auth",
      "session",
      "revoke",
      "--base-dir",
      baseDirectory,
      sessionId,
    ],
    { killSignal: "SIGKILL", timeout: 30_000 },
  );
}

async function waitForMarkers(rpc, probes) {
  const deadline = Date.now() + 60_000;
  do {
    if (
      (
        await Promise.all(
          probes.map((probe) => fileExists(probe.markerPath)),
        )
      ).every(Boolean)
    ) {
      return;
    }
    for (const probe of probes) {
      await rpc.request(
        "server.refreshProviders",
        { instanceId: probe.instanceId },
        { timeoutMs: 45_000 },
      );
    }
    await after(250);
  } while (Date.now() < deadline);
  throw new Error("provider processes did not restore persisted environments");
}

if (!t3Binary) {
  throw new Error("T3_PHASE0_T3_BIN is required");
}

if (mutationConsent !== "isolated") {
  throw new Error("Set T3_PHASE0_ALLOW_MUTATION=isolated for an isolated server");
}

const suffix = randomUUID().replaceAll("-", "").slice(0, 12);
const baseDirectory = await mkdtemp(join(tmpdir(), "t3-phase0-restart-base-"));
const stateRoot = await mkdtemp(join(tmpdir(), "t3-phase0-restart-state-"));
const port = await reservePort();
const httpBaseUrl = `http://127.0.0.1:${port}`;
const probes = [
  {
    driver: "codex",
    label: "Codex",
    binaryPath: codexBinary,
    homeEnvironmentName: "CODEX_HOME",
  },
  {
    driver: "claudeAgent",
    label: "Claude",
    binaryPath: claudeBinary,
    homeEnvironmentName: "CLAUDE_CONFIG_DIR",
  },
].map((definition) => {
  const driverSlug = definition.driver.toLowerCase();
  return {
    ...definition,
    instanceId: `phase0_${suffix}_${driverSlug}`,
    environmentName: `T3_PHASE0_${suffix.toUpperCase()}_${driverSlug.toUpperCase()}`,
    value: randomUUID(),
    homePath: join(stateRoot, `${driverSlug}-home`),
    wrapperPath: join(stateRoot, `${driverSlug}-capture.mjs`),
    markerPath: join(stateRoot, `${driverSlug}-observed`),
  };
});
for (const probe of probes) {
  probe.foreignEnvironmentName = probes.find(
    (candidate) => candidate.instanceId !== probe.instanceId,
  ).environmentName;
}

let serverProcess;
let rpc;
let adminSession;
let originalProviderInstances;
let originalUpdateChecks;
let successLine;

try {
  debug("writing provider wrappers");
  await Promise.all(
    probes.flatMap((probe) => [
      mkdir(probe.homePath, { recursive: true, mode: 0o700 }),
      writeCaptureWrapper(probe),
    ]),
  );

  debug("issuing administrative session");
  adminSession = await issueAdminSession(baseDirectory);
  debug("starting first server");
  serverProcess = startT3Server(port, baseDirectory);
  await waitForServer(serverProcess, httpBaseUrl);
  debug("first HTTP endpoint ready");
  await waitForAuthenticatedSession(httpBaseUrl, adminSession.token);
  rpc = await connectWhenReady(httpBaseUrl, adminSession.token);
  debug("reading initial settings");
  const originalSettings = await rpc.request("server.getSettings", {});
  originalProviderInstances = originalSettings.providerInstances;
  originalUpdateChecks = originalSettings.enableProviderUpdateChecks;
  const providerInstances = structuredClone(originalProviderInstances);
  for (const probe of probes) {
    providerInstances[probe.instanceId] = {
      driver: probe.driver,
      displayName: `Phase 0 restart ${probe.label}`,
      enabled: true,
      environment: [
        {
          name: probe.environmentName,
          value: probe.value,
          sensitive: true,
        },
      ],
      config: {
        binaryPath: probe.wrapperPath,
        homePath: probe.homePath,
        phase0Opaque: { survivesRestart: true },
      },
    };
  }
  const firstUpdate = await rpc.request(
    "server.updateSettings",
    {
      patch: {
        enableProviderUpdateChecks: false,
        providerInstances,
      },
    },
    { timeoutMs: 60_000 },
  );
  debug("first settings update complete");
  for (const probe of probes) {
    const environment =
      firstUpdate.providerInstances[probe.instanceId].environment[0];
    assert.equal(environment.value, "");
    assert.equal(environment.valueRedacted, true);
  }
  await waitForMarkers(rpc, probes);
  debug("first provider processes observed");
  await Promise.all(probes.map((probe) => rm(probe.markerPath)));

  rpc.close();
  rpc = undefined;
  await stopT3Server(serverProcess);
  serverProcess = undefined;

  debug("starting second server");
  serverProcess = startT3Server(port, baseDirectory);
  await waitForServer(serverProcess, httpBaseUrl);
  debug("second HTTP endpoint ready");
  await waitForAuthenticatedSession(httpBaseUrl, adminSession.token);
  rpc = await connectWhenReady(httpBaseUrl, adminSession.token);
  const restoredSettings = await rpc.request("server.getSettings", {});
  debug("restored settings read");
  for (const probe of probes) {
    const restored = restoredSettings.providerInstances[probe.instanceId];
    assert.equal(restored.driver, probe.driver);
    assert.equal(restored.enabled, true);
    assert.deepEqual(restored.config.phase0Opaque, { survivesRestart: true });
    assert.equal(restored.environment[0].value, "");
    assert.equal(restored.environment[0].valueRedacted, true);
  }
  await waitForMarkers(rpc, probes);
  debug("restored provider processes observed");
  await rpc.request(
    "server.updateSettings",
    {
      patch: {
        enableProviderUpdateChecks: originalUpdateChecks,
        providerInstances: originalProviderInstances,
      },
    },
    { timeoutMs: 60_000 },
  );
  successLine =
    "verified Codex and Claude settings, opaque config, sensitive environments, and provider processes across an idle t3 server restart";
} finally {
  rpc?.close();
  if (serverProcess) {
    await stopT3Server(serverProcess).catch(() => {});
  }
  if (adminSession?.sessionId) {
    await revokeAdminSession(baseDirectory, adminSession.sessionId).catch(
      () => {},
    );
  }
  await rm(baseDirectory, {
    recursive: true,
    force: true,
    maxRetries: 100,
    retryDelay: 100,
  });
  await rm(stateRoot, {
    recursive: true,
    force: true,
    maxRetries: 100,
    retryDelay: 100,
  });
}

process.stdout.write(`${successLine}\n`);
