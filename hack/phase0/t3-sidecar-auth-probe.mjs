import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const runFile = promisify(execFile);
const httpBaseUrl = process.env.T3_PHASE0_HTTP_URL ?? "http://127.0.0.1:39773";
const t3Binary = process.env.T3_PHASE0_T3_BIN;
const baseDirectory = process.env.T3_PHASE0_BASE_DIR;
const mutationConsent = process.env.T3_PHASE0_ALLOW_MUTATION;
const settingsProbe = fileURLToPath(
  new URL("./t3-live-settings-probe.mjs", import.meta.url),
);
const managedSettingsDriftProbe = fileURLToPath(
  new URL("./t3-managed-settings-drift-probe.mjs", import.meta.url),
);
const sensitiveEnvironmentProbe = fileURLToPath(
  new URL("./t3-sensitive-environment-probe.mjs", import.meta.url),
);
const restartPersistenceProbe = fileURLToPath(
  new URL("./t3-restart-persistence-probe.mjs", import.meta.url),
);
const providerRegistryProbe = fileURLToPath(
  new URL("./t3-provider-registry-probe.mjs", import.meta.url),
);
const labelSuffix = randomUUID().replaceAll("-", "").slice(0, 12);
const bootstrapLabel = `phase0-bootstrap-${labelSuffix}`;
const narrowLabel = `phase0-t3-coded-${labelSuffix}`;
const requiredScopes = ["orchestration:read", "orchestration:operate"];

if (!t3Binary) {
  throw new Error("T3_PHASE0_T3_BIN is required");
}

if (!baseDirectory) {
  throw new Error("T3_PHASE0_BASE_DIR is required");
}

if (mutationConsent !== "isolated") {
  throw new Error("Set T3_PHASE0_ALLOW_MUTATION=isolated for an isolated server");
}

async function runT3(arguments_) {
  return runFile(t3Binary, arguments_, { maxBuffer: 1024 * 1024 });
}

async function fetchJson(path, options = {}) {
  const response = await fetch(new URL(path, httpBaseUrl), options);
  if (!response.ok) {
    throw new Error(`${path} failed with ${response.status}`);
  }
  return response.json();
}

function authorizationHeaders(token, contentType) {
  return {
    Authorization: `Bearer ${token}`,
    ...(contentType ? { "Content-Type": contentType } : {}),
  };
}

async function issueBootstrapSession() {
  const result = await runT3([
    "auth",
    "session",
    "issue",
    "--base-dir",
    baseDirectory,
    "--ttl",
    "2m",
    "--label",
    bootstrapLabel,
    "--subject",
    "t3-coded-bootstrap",
    "--json",
  ]);
  const session = JSON.parse(result.stdout);
  assert.ok(session.scopes.includes("access:read"));
  assert.ok(session.scopes.includes("access:write"));
  assert.equal(typeof session.sessionId, "string");
  return session;
}

async function exchangeNarrowSession(bootstrapToken) {
  const pairing = await fetchJson("/api/auth/pairing-token", {
    method: "POST",
    headers: authorizationHeaders(bootstrapToken, "application/json"),
    body: JSON.stringify({ label: narrowLabel, scopes: requiredScopes }),
  });
  assert.equal(typeof pairing.credential, "string");
  assert.notEqual(pairing.credential.length, 0);

  const tokenRequest = new URLSearchParams({
    grant_type: "urn:ietf:params:oauth:grant-type:token-exchange",
    subject_token: pairing.credential,
    subject_token_type: "urn:t3:params:oauth:token-type:environment-bootstrap",
    requested_token_type: "urn:ietf:params:oauth:token-type:access_token",
    scope: requiredScopes.join(" "),
    client_label: narrowLabel,
  });
  const access = await fetchJson("/oauth/token", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: tokenRequest,
  });
  assert.equal(access.token_type, "Bearer");
  assert.ok(access.expires_in > 0);
  assert.deepEqual(access.scope.split(" ").sort(), requiredScopes.toSorted());
  return access;
}

async function getNarrowSessionState(accessToken) {
  return fetchJson("/api/auth/session", {
    headers: authorizationHeaders(accessToken),
  });
}

async function listNarrowSessions() {
  const result = await runT3([
    "auth",
    "session",
    "list",
    "--base-dir",
    baseDirectory,
    "--json",
  ]);
  return JSON.parse(result.stdout).filter(
    (session) => session.client.label === narrowLabel,
  );
}

async function cleanProbeCredentials() {
  const pairingList = await runT3([
    "auth",
    "pairing",
    "list",
    "--base-dir",
    baseDirectory,
    "--json",
  ]);
  const pairings = JSON.parse(pairingList.stdout);
  for (const pairing of pairings) {
    if (pairing.label === narrowLabel) {
      await runT3([
        "auth",
        "pairing",
        "revoke",
        "--base-dir",
        baseDirectory,
        pairing.id,
      ]);
    }
  }

  const sessionList = await runT3([
    "auth",
    "session",
    "list",
    "--base-dir",
    baseDirectory,
    "--json",
  ]);
  const sessions = JSON.parse(sessionList.stdout);
  for (const session of sessions) {
    if (
      session.client.label === bootstrapLabel ||
      session.client.label === narrowLabel
    ) {
      await runT3([
        "auth",
        "session",
        "revoke",
        "--base-dir",
        baseDirectory,
        session.sessionId,
      ]);
    }
  }

  const remainingPairings = JSON.parse(
    (
      await runT3([
        "auth",
        "pairing",
        "list",
        "--base-dir",
        baseDirectory,
        "--json",
      ])
    ).stdout,
  ).filter((pairing) => pairing.label === narrowLabel);
  const remainingSessions = JSON.parse(
    (
      await runT3([
        "auth",
        "session",
        "list",
        "--base-dir",
        baseDirectory,
        "--json",
      ])
    ).stdout,
  ).filter(
    (session) =>
      session.client.label === bootstrapLabel ||
      session.client.label === narrowLabel,
  );
  assert.equal(remainingPairings.length, 0);
  assert.equal(remainingSessions.length, 0);
}

const probeOutputs = [];

try {
  const bootstrap = await issueBootstrapSession();
  const initialAccess = await exchangeNarrowSession(bootstrap.token);
  const narrowSession = await getNarrowSessionState(initialAccess.access_token);
  assert.equal(narrowSession.authenticated, true);
  assert.deepEqual(narrowSession.scopes.toSorted(), requiredScopes.toSorted());

  const clientSessions = await fetchJson("/api/auth/clients", {
    headers: authorizationHeaders(bootstrap.token),
  });
  const registeredNarrowSession = clientSessions.find(
    (session) => session.client.label === narrowLabel,
  );
  assert.ok(registeredNarrowSession);
  assert.deepEqual(
    registeredNarrowSession.scopes.toSorted(),
    requiredScopes.toSorted(),
  );

  await runT3([
    "auth",
    "session",
    "revoke",
    "--base-dir",
    baseDirectory,
    bootstrap.sessionId,
  ]);
  const sessionsAfterBootstrapRevocation = JSON.parse(
    (
      await runT3([
        "auth",
        "session",
        "list",
        "--base-dir",
        baseDirectory,
        "--json",
      ])
    ).stdout,
  );
  assert.equal(
    sessionsAfterBootstrapRevocation.some(
      (session) => session.client.label === bootstrapLabel,
    ),
    false,
  );

  const initialNarrowSessions = await listNarrowSessions();
  assert.equal(initialNarrowSessions.length, 1);
  const renewalBootstrap = await issueBootstrapSession();
  const renewedAccess = await exchangeNarrowSession(renewalBootstrap.token);
  const renewedSession = await getNarrowSessionState(renewedAccess.access_token);
  assert.equal(renewedSession.authenticated, true);
  assert.deepEqual(renewedSession.scopes.toSorted(), requiredScopes.toSorted());
  assert.equal((await listNarrowSessions()).length, 2);

  await runT3([
    "auth",
    "session",
    "revoke",
    "--base-dir",
    baseDirectory,
    renewalBootstrap.sessionId,
  ]);
  await runT3([
    "auth",
    "session",
    "revoke",
    "--base-dir",
    baseDirectory,
    initialNarrowSessions[0].sessionId,
  ]);
  assert.equal(
    (await getNarrowSessionState(initialAccess.access_token)).authenticated,
    false,
  );
  assert.equal(
    (await getNarrowSessionState(renewedAccess.access_token)).authenticated,
    true,
  );

  const childEnvironment = {
    ...process.env,
    T3_PHASE0_BEARER: renewedAccess.access_token,
    T3_PHASE0_ALLOW_MUTATION: "isolated",
    T3_PHASE0_HTTP_URL: httpBaseUrl,
  };
  for (const probe of [
    settingsProbe,
    managedSettingsDriftProbe,
    sensitiveEnvironmentProbe,
    restartPersistenceProbe,
    providerRegistryProbe,
  ]) {
    probeOutputs.push(
      await runFile(process.execPath, [probe], {
        env: childEnvironment,
        maxBuffer: 1024 * 1024,
      }),
    );
  }
} finally {
  await cleanProbeCredentials();
}

for (const probeOutput of probeOutputs) {
  process.stdout.write(probeOutput.stdout);
  process.stderr.write(probeOutput.stderr);
}
process.stdout.write(
  "verified upstream bootstrap, immediate admin revocation, narrow orchestration scopes, credential renewal, prior-session revocation, settings authorization, and credential cleanup\n",
);
