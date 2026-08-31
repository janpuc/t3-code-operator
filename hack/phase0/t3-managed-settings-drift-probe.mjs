import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { isDeepStrictEqual } from "node:util";
import { connectT3Rpc } from "./t3-rpc-client.mjs";

const httpBaseUrl = process.env.T3_PHASE0_HTTP_URL ?? "http://127.0.0.1:39773";
const bearerToken = process.env.T3_PHASE0_BEARER;
const mutationConsent = process.env.T3_PHASE0_ALLOW_MUTATION;

function after(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

if (!bearerToken) {
  throw new Error("T3_PHASE0_BEARER is required");
}

if (mutationConsent !== "isolated") {
  throw new Error("Set T3_PHASE0_ALLOW_MUTATION=isolated for an isolated server");
}

const probeSuffix = randomUUID().replaceAll("-", "").slice(0, 12);
const desiredInstanceId = `phase0_${probeSuffix}_managed`;
const rogueInstanceId = `phase0_${probeSuffix}_rogue`;
const unrelatedSetting = `/tmp/t3-phase0-drift-${probeSuffix}`;
const rpc = await connectT3Rpc({ httpBaseUrl, bearerToken });
const originalSettings = await rpc.request("server.getSettings", {});

try {
  const desiredUpdate = await rpc.request("server.updateSettings", {
    patch: {
      providerInstances: {
        ...originalSettings.providerInstances,
        [desiredInstanceId]: {
          driver: "codex",
          displayName: "GitOps desired instance",
          enabled: false,
          config: {
            binaryPath: "codex",
            phase0Unknown: { preserved: true },
          },
        },
      },
    },
  });
  const desiredProviderInstances = desiredUpdate.providerInstances;
  let writerDone = false;
  let writerFinishedAt = 0;
  let reconciliationWrites = 0;
  const uiWriter = (async () => {
    for (let iteration = 0; iteration < 10; iteration += 1) {
      const driftedProviderInstances = structuredClone(desiredProviderInstances);
      driftedProviderInstances[desiredInstanceId].displayName = `UI drift ${iteration}`;
      driftedProviderInstances[rogueInstanceId] = {
        driver: "codex",
        displayName: `UI-only instance ${iteration}`,
        enabled: false,
        config: { binaryPath: "codex" },
      };
      await rpc.request("server.updateSettings", {
        patch: {
          addProjectBaseDirectory: unrelatedSetting,
          providerInstances: driftedProviderInstances,
        },
      });
      await after(20);
    }
    writerFinishedAt = Date.now();
    writerDone = true;
  })();
  const reconciler = (async () => {
    const deadline = Date.now() + 10_000;
    do {
      const currentSettings = await rpc.request("server.getSettings", {});
      if (
        !isDeepStrictEqual(
          currentSettings.providerInstances,
          desiredProviderInstances,
        )
      ) {
        await rpc.request("server.updateSettings", {
          patch: { providerInstances: desiredProviderInstances },
        });
        reconciliationWrites += 1;
      } else if (writerDone) {
        return Date.now() - writerFinishedAt;
      }
      await after(10);
    } while (Date.now() < deadline);
    throw new Error("managed provider map did not converge within 10 seconds");
  })();
  const [, convergenceMilliseconds] = await Promise.all([uiWriter, reconciler]);

  const observedSettings = await rpc.request("server.getSettings", {});
  assert.deepEqual(observedSettings.providerInstances, desiredProviderInstances);
  assert.equal(observedSettings.addProjectBaseDirectory, unrelatedSetting);
  assert.ok(convergenceMilliseconds >= 0 && convergenceMilliseconds <= 5_000);
  assert.deepEqual(
    observedSettings.providerInstances[desiredInstanceId].config.phase0Unknown,
    { preserved: true },
  );
  process.stdout.write(
    `verified authoritative provider-map convergence after 10 competing writes in ${convergenceMilliseconds}ms with ${reconciliationWrites} corrective writes while preserving unrelated settings and unknown desired config\n`,
  );
} finally {
  try {
    await rpc.request("server.updateSettings", {
      patch: {
        addProjectBaseDirectory: originalSettings.addProjectBaseDirectory,
        providerInstances: originalSettings.providerInstances,
      },
    });
  } finally {
    rpc.close();
  }
}
