import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { connectT3Rpc } from "./t3-rpc-client.mjs";

const httpBaseUrl = process.env.T3_PHASE0_HTTP_URL ?? "http://127.0.0.1:39773";
const bearerToken = process.env.T3_PHASE0_BEARER;
const mutationConsent = process.env.T3_PHASE0_ALLOW_MUTATION;

if (!bearerToken) {
  throw new Error("T3_PHASE0_BEARER is required");
}

if (mutationConsent !== "isolated") {
  throw new Error("Set T3_PHASE0_ALLOW_MUTATION=isolated for an isolated server");
}

const rpc = await connectT3Rpc({ httpBaseUrl, bearerToken });
const { request } = rpc;

const originalSettings = await request("server.getSettings", {});
const originalProviderInstances = originalSettings.providerInstances;
const originalProjectBaseDirectory = originalSettings.addProjectBaseDirectory;
const probeSuffix = randomUUID().replaceAll("-", "").slice(0, 12);
const firstInstanceId = `phase0_${probeSuffix}_one`;
const secondInstanceId = `phase0_${probeSuffix}_two`;
const secretEnvironmentName = `T3_PHASE0_${probeSuffix.toUpperCase()}`;
const initialSecretValue = randomUUID();
const rotatedSecretValue = randomUUID();
const projectBaseDirectoryMarker = `/tmp/t3-phase0-${probeSuffix}`;

const firstInstance = {
  driver: "codex",
  displayName: "Phase 0 instance one",
  enabled: false,
  environment: [
    {
      name: secretEnvironmentName,
      value: initialSecretValue,
      sensitive: true,
    },
  ],
  config: {
    binaryPath: "codex",
    phase0Unknown: { preserved: true },
  },
};

const secondInstance = {
  driver: "codex",
  displayName: "Phase 0 instance two",
  enabled: false,
  config: {
    binaryPath: "codex",
    phase0Unknown: { preserved: true },
  },
};

try {
  const firstUpdate = await request("server.updateSettings", {
    patch: {
      addProjectBaseDirectory: projectBaseDirectoryMarker,
      providerInstances: {
        ...originalProviderInstances,
        [firstInstanceId]: firstInstance,
        [secondInstanceId]: secondInstance,
      },
    },
  });

  assert.equal(firstUpdate.addProjectBaseDirectory, projectBaseDirectoryMarker);
  assert.deepEqual(firstUpdate.providerInstances[firstInstanceId].config.phase0Unknown, {
    preserved: true,
  });
  assert.equal(firstUpdate.providerInstances[firstInstanceId].environment[0].value, "");
  assert.equal(
    firstUpdate.providerInstances[firstInstanceId].environment[0].valueRedacted,
    true,
  );
  assert.ok(firstUpdate.providerInstances[secondInstanceId]);

  const mapWithOneProbeInstance = structuredClone(firstUpdate.providerInstances);
  delete mapWithOneProbeInstance[secondInstanceId];
  const secondUpdate = await request("server.updateSettings", {
    patch: { providerInstances: mapWithOneProbeInstance },
  });

  assert.equal(secondUpdate.addProjectBaseDirectory, projectBaseDirectoryMarker);
  assert.ok(secondUpdate.providerInstances[firstInstanceId]);
  assert.equal(secondUpdate.providerInstances[secondInstanceId], undefined);
  assert.equal(
    secondUpdate.providerInstances[firstInstanceId].environment[0].valueRedacted,
    true,
  );

  const restoredProbeMap = structuredClone(secondUpdate.providerInstances);
  restoredProbeMap[secondInstanceId] = secondInstance;
  const restoredProbeUpdate = await request("server.updateSettings", {
    patch: { providerInstances: restoredProbeMap },
  });

  const rotatedProbeMap = structuredClone(restoredProbeUpdate.providerInstances);
  rotatedProbeMap[firstInstanceId].environment = [
    {
      name: secretEnvironmentName,
      value: rotatedSecretValue,
      sensitive: true,
    },
  ];
  const rotationUpdate = await request("server.updateSettings", {
    patch: { providerInstances: rotatedProbeMap },
  });

  assert.equal(rotationUpdate.providerInstances[firstInstanceId].environment[0].value, "");
  assert.equal(
    rotationUpdate.providerInstances[firstInstanceId].environment[0].valueRedacted,
    true,
  );

  const observedSettings = await request("server.getSettings", {});
  assert.equal(observedSettings.addProjectBaseDirectory, projectBaseDirectoryMarker);
  assert.ok(observedSettings.providerInstances[firstInstanceId]);
  assert.ok(observedSettings.providerInstances[secondInstanceId]);
  assert.equal(
    observedSettings.providerInstances[firstInstanceId].environment[0].valueRedacted,
    true,
  );

  process.stdout.write(
    "verified authenticated settings update, upstream-slug instance IDs, opaque config preservation, whole-map replacement, unrelated setting preservation, and sensitive value rotation\n",
  );
} finally {
  try {
    await request("server.updateSettings", {
      patch: {
        addProjectBaseDirectory: originalProjectBaseDirectory,
        providerInstances: originalProviderInstances,
      },
    });
  } finally {
    rpc.close();
  }
}
