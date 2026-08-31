function decodeSocketData(data) {
  if (typeof data === "string") {
    return data;
  }
  if (data instanceof ArrayBuffer) {
    return Buffer.from(data).toString("utf8");
  }
  return Buffer.from(data).toString("utf8");
}

export async function connectT3Rpc({
  httpBaseUrl,
  bearerToken,
  connectTimeoutMs = 15_000,
}) {
  const ticketResponse = await fetch(
    new URL("/api/auth/websocket-ticket", httpBaseUrl),
    {
      method: "POST",
      headers: { Authorization: `Bearer ${bearerToken}` },
      signal: AbortSignal.timeout(connectTimeoutMs),
    },
  );
  if (!ticketResponse.ok) {
    throw new Error(`WebSocket ticket request failed with ${ticketResponse.status}`);
  }

  const { ticket } = await ticketResponse.json();
  if (typeof ticket !== "string" || ticket.length === 0) {
    throw new Error("WebSocket ticket response was invalid");
  }

  const socketUrl = new URL("/ws", httpBaseUrl);
  socketUrl.protocol = socketUrl.protocol === "https:" ? "wss:" : "ws:";
  socketUrl.searchParams.set("wsTicket", ticket);

  const socket = new WebSocket(socketUrl);
  const pendingRequests = new Map();
  let nextRequestId = 1;

  function rejectPendingRequests(error) {
    for (const { reject, timeout } of pendingRequests.values()) {
      clearTimeout(timeout);
      reject(error);
    }
    pendingRequests.clear();
  }

  socket.addEventListener("message", (event) => {
    const decoded = JSON.parse(decodeSocketData(event.data));
    const messages = Array.isArray(decoded) ? decoded : [decoded];

    for (const message of messages) {
      if (message._tag === "Ping") {
        socket.send(JSON.stringify({ _tag: "Pong" }));
        continue;
      }
      if (message._tag === "Defect") {
        rejectPendingRequests(
          new Error("The t3 RPC server returned a protocol defect"),
        );
        continue;
      }
      if (message._tag !== "Exit") {
        continue;
      }

      const pending = pendingRequests.get(message.requestId);
      if (!pending) {
        continue;
      }

      pendingRequests.delete(message.requestId);
      clearTimeout(pending.timeout);
      if (message.exit?._tag === "Success") {
        pending.resolve(message.exit.value);
      } else {
        pending.reject(new Error(`t3 RPC failed: ${JSON.stringify(message.exit)}`));
      }
    }
  });
  socket.addEventListener("close", () => {
    rejectPendingRequests(new Error("The t3 RPC socket closed"));
  });
  socket.addEventListener("error", () => {
    rejectPendingRequests(new Error("The t3 RPC socket failed"));
  });

  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      socket.close();
      reject(new Error("The t3 RPC socket connection timed out"));
    }, connectTimeoutMs);
    socket.addEventListener(
      "open",
      () => {
        clearTimeout(timeout);
        resolve();
      },
      { once: true },
    );
    socket.addEventListener(
      "error",
      () => {
        clearTimeout(timeout);
        reject(new Error("The t3 RPC socket failed"));
      },
      { once: true },
    );
    socket.addEventListener(
      "close",
      () => {
        clearTimeout(timeout);
        reject(new Error("The t3 RPC socket closed before it connected"));
      },
      { once: true },
    );
  });

  function request(tag, payload, { timeoutMs = 30_000 } = {}) {
    const id = nextRequestId;
    nextRequestId += 1;

    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        pendingRequests.delete(id);
        reject(new Error(`t3 RPC timed out: ${tag}`));
      }, timeoutMs);

      pendingRequests.set(id, { resolve, reject, timeout });
      socket.send(
        JSON.stringify({
          _tag: "Request",
          id,
          tag,
          payload,
          headers: [],
        }),
      );
    });
  }

  function close() {
    if (socket.readyState === WebSocket.OPEN) {
      socket.close();
    }
  }

  return { close, request };
}
