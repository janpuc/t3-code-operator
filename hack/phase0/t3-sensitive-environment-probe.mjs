import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import {
  access,
  chmod,
  mkdtemp,
  rename,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connectT3Rpc } from "./t3-rpc-client.mjs";

const httpBaseUrl = process.env.T3_PHASE0_HTTP_URL ?? "http://127.0.0.1:39773";
const bearerToken = process.env.T3_PHASE0_BEARER;
const mutationConsent = process.env.T3_PHASE0_ALLOW_MUTATION;
const codexBinary = process.env.T3_PHASE0_CODEX_BIN ?? "codex";
const claudeBinary = process.env.T3_PHASE0_CLAUDE_BIN ?? "claude";

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

function captureWrapperSource(input) {
  return [
    "#!/usr/bin/env node",
    'import { createHash } from "node:crypto";',
    'import { writeFileSync } from "node:fs";',
    'import { spawnSync } from "node:child_process";',
    `const value = process.env[${JSON.stringify(input.environmentName)}] ?? "";`,
    'const digest = createHash("sha256").update(value).digest("hex");',
    `const foreignNames = ${JSON.stringify(input.foreignEnvironmentNames)};`,
    "const foreignValuesAbsent = foreignNames.every((name) => process.env[name] === undefined);",
    `const homeMatches = process.env[${JSON.stringify(input.homeEnvironmentName)}] === ${JSON.stringify(input.expectedHome)};`,
    `if (digest === ${JSON.stringify(input.expectedHash)} && foreignValuesAbsent && homeMatches) {`,
    `  writeFileSync(${JSON.stringify(input.markerPath)}, "", { mode: 0o600 });`,
    "}",
    `const child = spawnSync(${JSON.stringify(input.binaryPath)}, process.argv.slice(2), { stdio: "inherit" });`,
    "if (child.error) throw child.error;",
    "process.exit(child.status ?? 1);",
    "",
  ].join("\n");
}

async function writeCaptureWrapper(path, input) {
  const nextPath = `${path}.next`;
  await writeFile(nextPath, captureWrapperSource(input), { mode: 0o700 });
  await chmod(nextPath, 0o700);
  await rename(nextPath, path);
}

async function waitForMarker(rpc, instanceId, markerPath) {
  const deadline = Date.now() + 60_000;
  do {
    if (await fileExists(markerPath)) {
      return;
    }
    await rpc.request(
      "server.refreshProviders",
      { instanceId },
      { timeoutMs: 45_000 },
    );
    if (await fileExists(markerPath)) {
      return;
    }
    await after(250);
  } while (Date.now() < deadline);
  throw new Error(`provider process did not materialize environment for ${instanceId}`);
}

async function waitForRemoval(rpc, instanceIds) {
  const expectedAbsent = new Set(instanceIds);
  const deadline = Date.now() + 60_000;
  do {
    const config = await rpc.request("server.getConfig", {}, { timeoutMs: 15_000 });
    if (
      config.providers.every(
        (provider) => !expectedAbsent.has(provider.instanceId),
      )
    ) {
      return;
    }
    await after(250);
  } while (Date.now() < deadline);
  throw new Error("provider registry did not remove sensitive environment probes");
}

if (!bearerToken) {
  throw new Error("T3_PHASE0_BEARER is required");
}

if (mutationConsent !== "isolated") {
  throw new Error("Set T3_PHASE0_ALLOW_MUTATION=isolated for an isolated server");
}

const probeSuffix = randomUUID().replaceAll("-", "").slice(0, 12);
const stateRoot = await mkdtemp(join(tmpdir(), "t3-phase0-sensitive-env-"));
const probeDefinitions = [
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
];
const probeInstances = probeDefinitions.flatMap((definition) =>
  ["a", "b"].map((slot) => {
    const driverSlug = definition.driver.toLowerCase();
    const stem = `${driverSlug}-${slot}`;
    return {
      ...definition,
      slot,
      instanceId: `phase0_${probeSuffix}_${driverSlug}_${slot}`,
      environmentName: `T3_PHASE0_${probeSuffix.toUpperCase()}_${driverSlug.toUpperCase()}_${slot.toUpperCase()}`,
      initialValue: randomUUID(),
      rotatedValue: slot === "a" ? randomUUID() : undefined,
      homePath: join(stateRoot, stem),
      wrapperPath: join(stateRoot, `${stem}-capture.mjs`),
      markerPath: join(stateRoot, `${stem}-observed`),
    };
  }),
);
let rpc;
let originalProviderInstances;
let originalUpdateChecks;
let settingsRestoreRequired = false;
let successLine;

try {
  await Promise.all(
    probeInstances.map((probe) =>
      writeCaptureWrapper(probe.wrapperPath, {
        binaryPath: probe.binaryPath,
        environmentName: probe.environmentName,
        foreignEnvironmentNames: probeInstances
          .filter((candidate) => candidate.instanceId !== probe.instanceId)
          .map((candidate) => candidate.environmentName),
        homeEnvironmentName: probe.homeEnvironmentName,
        expectedHash: sha256(probe.initialValue),
        expectedHome: probe.homePath,
        markerPath: probe.markerPath,
      }),
    ),
  );

  rpc = await connectT3Rpc({ httpBaseUrl, bearerToken });
  const originalSettings = await rpc.request("server.getSettings", {});
  originalProviderInstances = originalSettings.providerInstances;
  originalUpdateChecks = originalSettings.enableProviderUpdateChecks;
  settingsRestoreRequired = true;

  const providerInstances = structuredClone(originalProviderInstances);
  for (const probe of probeInstances) {
    providerInstances[probe.instanceId] = {
      driver: probe.driver,
      displayName: `Phase 0 ${probe.label} ${probe.slot.toUpperCase()}`,
      enabled: true,
      environment: [
        {
          name: probe.environmentName,
          value: probe.initialValue,
          sensitive: true,
        },
      ],
      config: { binaryPath: probe.wrapperPath, homePath: probe.homePath },
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

  for (const probe of probeInstances) {
    const variable =
      firstUpdate.providerInstances[probe.instanceId].environment[0];
    assert.equal(variable.value, "");
    assert.equal(variable.valueRedacted, true);
  }
  await Promise.all(
    probeInstances.map((probe) =>
      waitForMarker(rpc, probe.instanceId, probe.markerPath),
    ),
  );

  const rotatingProbes = probeInstances.filter(
    (probe) => probe.rotatedValue !== undefined,
  );
  await Promise.all(
    rotatingProbes.map(async (probe) => {
      await rm(probe.markerPath, { force: true });
      await writeCaptureWrapper(probe.wrapperPath, {
        binaryPath: probe.binaryPath,
        environmentName: probe.environmentName,
        foreignEnvironmentNames: probeInstances
          .filter((candidate) => candidate.instanceId !== probe.instanceId)
          .map((candidate) => candidate.environmentName),
        homeEnvironmentName: probe.homeEnvironmentName,
        expectedHash: sha256(probe.rotatedValue),
        expectedHome: probe.homePath,
        markerPath: probe.markerPath,
      });
    }),
  );
  const rotatedProviderInstances = structuredClone(firstUpdate.providerInstances);
  for (const probe of rotatingProbes) {
    rotatedProviderInstances[probe.instanceId].environment = [
      {
        name: probe.environmentName,
        value: probe.rotatedValue,
        sensitive: true,
      },
    ];
  }
  const rotationUpdate = await rpc.request(
    "server.updateSettings",
    { patch: { providerInstances: rotatedProviderInstances } },
    { timeoutMs: 60_000 },
  );
  for (const probe of rotatingProbes) {
    const variable =
      rotationUpdate.providerInstances[probe.instanceId].environment[0];
    assert.equal(variable.value, "");
    assert.equal(variable.valueRedacted, true);
  }
  await Promise.all(
    rotatingProbes.map((probe) =>
      waitForMarker(rpc, probe.instanceId, probe.markerPath),
    ),
  );
  successLine =
    "verified two Codex and two Claude home paths, isolated process environments, sensitive-value redaction, and rotated-value injection into new provider processes";
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
      await waitForRemoval(
        rpc,
        probeInstances.map((probe) => probe.instanceId),
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

process.stdout.write(`${successLine}\n`);
