import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { connectT3Rpc } from "./t3-rpc-client.mjs";

const runFile = promisify(execFile);
const httpBaseUrl = process.env.T3_PHASE0_HTTP_URL ?? "http://127.0.0.1:39773";
const bearerToken = process.env.T3_PHASE0_BEARER;
const mutationConsent = process.env.T3_PHASE0_ALLOW_MUTATION;
const openCodeServerUrl =
  process.env.T3_PHASE0_OPENCODE_SERVER_URL ?? "http://127.0.0.1:1";
const driverCommands = {
  codex: process.env.T3_PHASE0_CODEX_BIN ?? "codex",
  claudeAgent: process.env.T3_PHASE0_CLAUDE_BIN ?? "claude",
  cursor: process.env.T3_PHASE0_CURSOR_BIN ?? "cursor-agent",
  grok: process.env.T3_PHASE0_GROK_BIN ?? "grok",
  opencode: process.env.T3_PHASE0_OPENCODE_BIN ?? "opencode",
};

function after(milliseconds, value) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds, value));
}

async function verifyGrokStdioLaunch(binaryPath, homePath) {
  const child = spawn(binaryPath, ["agent", "stdio"], {
    env: { ...process.env, HOME: homePath },
    stdio: ["pipe", "pipe", "pipe"],
  });
  let stderr = "";
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => {
    stderr += chunk;
  });
  child.stdout.resume();
  child.stdin.on("error", () => {});

  const completion = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      resolve({ state: "exited", code, signal });
    });
  });
  child.stdin.end();

  const initialOutcome = await Promise.race([
    completion,
    after(1_000, { state: "running" }),
  ]);
  if (initialOutcome.state === "exited") {
    assert.equal(initialOutcome.code, 0, stderr);
    return "exited cleanly after stdin closed";
  }

  child.kill("SIGTERM");
  const terminationOutcome = await Promise.race([
    completion,
    after(5_000, { state: "running" }),
  ]);
  if (terminationOutcome.state === "running") {
    child.kill("SIGKILL");
    await completion;
  }
  return "remained active until the probe terminated it";
}

async function waitForProviderSnapshot(rpc, instanceId) {
  const deadline = Date.now() + 60_000;
  let lastSnapshot;
  do {
    let refreshed;
    try {
      refreshed = await rpc.request(
        "server.refreshProviders",
        { instanceId },
        { timeoutMs: 45_000 },
      );
    } catch (error) {
      throw new Error(`provider refresh failed for ${instanceId}`, {
        cause: error,
      });
    }
    const snapshot = refreshed.providers.find(
      (provider) => provider.instanceId === instanceId,
    );
    if (snapshot?.installed === true) {
      return snapshot;
    }
    lastSnapshot = snapshot;
    await after(250);
  } while (Date.now() < deadline);
  return lastSnapshot;
}

async function waitForProviderRemoval(rpc, instanceIds) {
  const probeInstanceIds = new Set(instanceIds);
  const deadline = Date.now() + 60_000;
  let probeProviders = [];
  do {
    const config = await rpc.request(
      "server.getConfig",
      {},
      { timeoutMs: 15_000 },
    );
    probeProviders = config.providers.filter((provider) =>
      probeInstanceIds.has(provider.instanceId),
    );
    if (probeProviders.length === 0) {
      return [];
    }
    await after(250);
  } while (Date.now() < deadline);
  return probeProviders.map((provider) => ({
    instanceId: provider.instanceId,
    driver: provider.driver,
    status: provider.status,
  }));
}

if (!bearerToken) {
  throw new Error("T3_PHASE0_BEARER is required");
}

if (mutationConsent !== "isolated") {
  throw new Error("Set T3_PHASE0_ALLOW_MUTATION=isolated for an isolated server");
}

const probeSuffix = randomUUID().replaceAll("-", "").slice(0, 12);
const stateRoot = await mkdtemp(join(tmpdir(), "t3-phase0-drivers-"));
const probeInstances = {};

for (const [driver, binaryPath] of Object.entries(driverCommands)) {
  const instanceId = `phase0_${probeSuffix}_${driver}`;
  const instanceRoot = join(stateRoot, driver);
  await mkdir(instanceRoot, { recursive: true });
  probeInstances[instanceId] = {
    driver,
    displayName: `Phase 0 ${driver}`,
    enabled: true,
    environment: [
      { name: "HOME", value: instanceRoot, sensitive: false },
      {
        name: "XDG_CACHE_HOME",
        value: join(instanceRoot, ".cache"),
        sensitive: false,
      },
      {
        name: "XDG_CONFIG_HOME",
        value: join(instanceRoot, ".config"),
        sensitive: false,
      },
      {
        name: "XDG_DATA_HOME",
        value: join(instanceRoot, ".local", "share"),
        sensitive: false,
      },
    ],
    config: {
      binaryPath,
      ...(driver === "codex" || driver === "claudeAgent"
        ? { homePath: join(instanceRoot, "provider-home") }
        : {}),
      ...(driver === "opencode" ? { serverUrl: openCodeServerUrl } : {}),
      phase0Unknown: { preserved: true },
    },
  };
}

const executableVersions = {};
const successLines = [];
let rpc;
let originalProviderInstances;
let originalUpdateChecks;
let settingsRestoreRequired = false;

try {
  rpc = await connectT3Rpc({ httpBaseUrl, bearerToken });
  const originalSettings = await rpc.request("server.getSettings", {});
  originalProviderInstances = originalSettings.providerInstances;
  originalUpdateChecks = originalSettings.enableProviderUpdateChecks;

  for (const [driver, binaryPath] of Object.entries(driverCommands)) {
    const versionResult = await runFile(binaryPath, ["--version"], {
      env: { ...process.env, HOME: join(stateRoot, driver) },
      timeout: 15_000,
      maxBuffer: 1024 * 1024,
    });
    const versionOutput = `${versionResult.stdout}\n${versionResult.stderr}`.trim();
    assert.notEqual(versionOutput.length, 0);
    executableVersions[driver] = versionOutput.split("\n", 1)[0];
  }

  const grokStdioResult = await verifyGrokStdioLaunch(
    driverCommands.grok,
    join(stateRoot, "grok"),
  );

  settingsRestoreRequired = true;
  await rpc.request(
    "server.updateSettings",
    {
      patch: {
        enableProviderUpdateChecks: false,
        providerInstances: {
          ...originalProviderInstances,
          ...probeInstances,
        },
      },
    },
    { timeoutMs: 60_000 },
  );

  const snapshots = [];
  for (const [instanceId, instance] of Object.entries(probeInstances)) {
    const snapshot = await waitForProviderSnapshot(rpc, instanceId);
    assert.ok(snapshot, `provider registry did not materialize ${instanceId}`);
    assert.equal(snapshot.driver, instance.driver);
    assert.equal(snapshot.enabled, true);
    assert.equal(snapshot.installed, true);
    assert.equal(snapshot.availability ?? "available", "available");
    snapshots.push({
      driver: snapshot.driver,
      installed: snapshot.installed,
      status: snapshot.status,
      auth: snapshot.auth.status,
      version: snapshot.version,
    });
  }

  const observedSettings = await rpc.request("server.getSettings", {});
  for (const instanceId of Object.keys(probeInstances)) {
    assert.deepEqual(
      observedSettings.providerInstances[instanceId].config.phase0Unknown,
      { preserved: true },
    );
  }

  successLines.push(
    `verified five executable versions; Grok stdio ${grokStdioResult}: ${JSON.stringify(executableVersions)}`,
  );
  successLines.push(
    `verified five upstream provider registrations with isolated provider environments: ${JSON.stringify(snapshots)}`,
  );
} finally {
  try {
    if (rpc && settingsRestoreRequired) {
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
      const remainingProviders = await waitForProviderRemoval(
        rpc,
        Object.keys(probeInstances),
      );
      assert.deepEqual(
        remainingProviders,
        [],
        `provider registry did not remove every probe instance: ${JSON.stringify(remainingProviders)}`,
      );
    }
  } finally {
    rpc?.close();
    await rm(stateRoot, {
      recursive: true,
      force: true,
      maxRetries: 100,
      retryDelay: 100,
    });
  }
}

for (const line of successLines) {
  process.stdout.write(`${line}\n`);
}
