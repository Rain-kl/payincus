/**
 * PayIncus Admin Control Plane Bridge for Built-in Incus UI
 *
 * 1. Shares the single Admin JWT session from same-origin localStorage ('token').
 * 2. Rewrites all /1.0 REST requests (fetch & XMLHttpRequest) to
 *    /api/admin/hosts/:hostId/incus-proxy/1.0/* with Authorization header.
 * 3. Pre-fetches and auto-renews a short-lived capability ticket so synchronous
 *    new WebSocket() calls for /1.0/events, terminal exec, and SPICE VGA console
 *    are transparently routed to /api/ws/hosts/:hostId/incus-proxy/1.0/* over the reverse tunnel.
 */

const SUBPROTOCOL_PREFIX = "incudal-incus-ui.";
const TICKET_REFRESH_INTERVAL_MS = 3 * 60 * 1000;

function extractHostIdFromLocation(): number | null {
  if (typeof window === "undefined") return null;
  const match = window.location.pathname.match(/^\/incus-ui\/([1-9]\d*)(?:\/|$)/);
  if (match) {
    const parsed = Number(match[1]);
    if (Number.isSafeInteger(parsed) && parsed > 0) {
      return parsed;
    }
  }
  const queryHostId = new URLSearchParams(window.location.search).get("hostId");
  if (queryHostId && /^[1-9]\d*$/.test(queryHostId)) {
    const parsed = Number(queryHostId);
    if (Number.isSafeInteger(parsed) && parsed > 0) {
      return parsed;
    }
  }
  return null;
}

function getAdminAccessToken(): string | null {
  try {
    return window.localStorage.getItem("token");
  } catch {
    return null;
  }
}

function redirectToAdminLogin(): void {
  const redirectTarget = encodeURIComponent(
    window.location.pathname + window.location.search,
  );
  window.location.replace(`/admin/login?redirect=${redirectTarget}`);
}

/**
 * Extract the `/1.0...` portion from a relative or same-origin URL if it targets the Incus API.
 */
function extractIncusApiPath(rawUrl: string, rootPath: string): string | null {
  try {
    const parsed = new URL(rawUrl, window.location.origin);
    if (parsed.origin !== window.location.origin) {
      return null;
    }

    const pathname = parsed.pathname;
    const search = parsed.search || "";

    if (rootPath && (pathname === `${rootPath}/1.0` || pathname.startsWith(`${rootPath}/1.0/`))) {
      return pathname.slice(rootPath.length) + search;
    }

    const hostPrefixMatch = pathname.match(/^\/incus-ui\/[1-9]\d*(\/1\.0(?:\/.*)?)$/);
    if (hostPrefixMatch) {
      return hostPrefixMatch[1] + search;
    }

    if (pathname === "/1.0" || pathname.startsWith("/1.0/")) {
      return pathname + search;
    }

    return null;
  } catch {
    return null;
  }
}

let bridgeInitialized = false;

export function initPayIncusBridge(): void {
  if (typeof window === "undefined" || bridgeInitialized) return;
  bridgeInitialized = true;

  const hostId = extractHostIdFromLocation();
  if (!hostId) {
    return;
  }

  const rootPath = `/incus-ui/${hostId}`;
  window.ROOT_PATH = rootPath;

  if (
    window.location.pathname === rootPath ||
    window.location.pathname === `${rootPath}/`
  ) {
    window.history.replaceState(null, "", `${rootPath}/ui`);
  }

  const initialToken = getAdminAccessToken();
  if (!initialToken) {
    redirectToAdminLogin();
    return;
  }

  window.addEventListener("storage", (event) => {
    if (event.key === "token" && !event.newValue) {
      redirectToAdminLogin();
    }
  });

  const nativeFetch = window.fetch.bind(window);
  let cachedTicket: string | null = null;
  let ticketPromise: Promise<string | null> | null = null;

  const refreshTicket = async (): Promise<string | null> => {
    const token = getAdminAccessToken();
    if (!token) {
      redirectToAdminLogin();
      return null;
    }

    try {
      const response = await nativeFetch(
        `/api/admin/hosts/${hostId}/incus-ui-ticket`,
        {
          method: "POST",
          headers: {
            Authorization: `Bearer ${token}`,
            "Content-Type": "application/json",
          },
          body: "{}",
        },
      );

      if (response.status === 401 || response.status === 403) {
        redirectToAdminLogin();
        return null;
      }

      if (!response.ok) {
        return cachedTicket;
      }

      const data = (await response.json()) as { ticket?: string };
      if (typeof data.ticket === "string" && data.ticket.length > 0) {
        cachedTicket = data.ticket;
        return cachedTicket;
      }
      return cachedTicket;
    } catch {
      return cachedTicket;
    } finally {
      ticketPromise = null;
    }
  };

  const ensureTicket = (): Promise<string | null> => {
    if (cachedTicket) {
      return Promise.resolve(cachedTicket);
    }
    if (!ticketPromise) {
      ticketPromise = refreshTicket();
    }
    return ticketPromise;
  };

  // Start pre-fetching the ticket immediately at boot
  void ensureTicket();
  window.setInterval(() => {
    void refreshTicket();
  }, TICKET_REFRESH_INTERVAL_MS);

  // 1. Patch window.fetch
  window.fetch = async (
    input: RequestInfo | URL,
    init?: RequestInit,
  ): Promise<Response> => {
    const rawUrl =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.toString()
          : input.url;

    try {
      const parsedDoc = new URL(rawUrl, window.location.origin);
      if (
        parsedDoc.origin === window.location.origin &&
        parsedDoc.pathname.endsWith("/documentation/objects.inv.txt")
      ) {
        return new Response("", {
          status: 200,
          headers: { "Content-Type": "text/plain; charset=utf-8" },
        });
      }
    } catch {
      // ignore URL parse error
    }

    const incusPath = extractIncusApiPath(rawUrl, rootPath);
    if (!incusPath) {
      return nativeFetch(input, init);
    }

    // Ensure the WS/download capability ticket is warm before /1.0 settings resolves
    if (!cachedTicket) {
      await ensureTicket();
    }

    const token = getAdminAccessToken();
    if (!token) {
      redirectToAdminLogin();
      throw new Error("Admin authentication required");
    }

    const proxyUrl = `/api/admin/hosts/${hostId}/incus-proxy${incusPath}`;
    const headers = new Headers(
      init?.headers || (input instanceof Request ? input.headers : undefined),
    );
    headers.set("Authorization", `Bearer ${token}`);

    const response = await nativeFetch(proxyUrl, {
      ...init,
      headers,
    });

    if (response.status === 401) {
      redirectToAdminLogin();
    }

    return response;
  };

  // 2. Patch XMLHttpRequest (used by axios for ISO / backup / instance file uploads)
  const NativeXHR = window.XMLHttpRequest;
  const nativeOpen = NativeXHR.prototype.open;
  const nativeSend = NativeXHR.prototype.send;

  interface PatchedXHR extends XMLHttpRequest {
    __payincusProxyRewritten?: boolean;
  }

  NativeXHR.prototype.open = function (
    this: PatchedXHR,
    method: string,
    url: string | URL,
    async: boolean = true,
    username?: string | null,
    password?: string | null,
  ) {
    const rawUrl = typeof url === "string" ? url : url.toString();
    const incusPath = extractIncusApiPath(rawUrl, rootPath);
    if (incusPath) {
      this.__payincusProxyRewritten = true;
      const proxyUrl = `/api/admin/hosts/${hostId}/incus-proxy${incusPath}`;
      return nativeOpen.call(this, method, proxyUrl, async, username, password);
    }
    this.__payincusProxyRewritten = false;
    return nativeOpen.call(this, method, url, async, username, password);
  };

  NativeXHR.prototype.send = function (
    this: PatchedXHR,
    body?: Document | XMLHttpRequestBodyInit | null,
  ) {
    if (this.__payincusProxyRewritten) {
      const token = getAdminAccessToken();
      if (token) {
        try {
          this.setRequestHeader("Authorization", `Bearer ${token}`);
        } catch {
          // ignore if headers already sent
        }
      }
    }
    return nativeSend.call(this, body);
  };

  // 3. Patch window.WebSocket (used by /1.0/events, InstanceTerminal, InstanceGraphicConsole)
  const NativeWebSocket = window.WebSocket;
  const PatchedWebSocket = function (
    url: string | URL,
    protocols?: string | string[],
  ): WebSocket {
    const rawUrl = typeof url === "string" ? url : url.toString();
    const normalizedForParse = rawUrl.replace(/^wss?:/i, window.location.protocol);
    const incusPath = extractIncusApiPath(normalizedForParse, rootPath);

    if (!incusPath) {
      return protocols !== undefined
        ? new NativeWebSocket(url, protocols)
        : new NativeWebSocket(url);
    }

    const wsProto = window.location.protocol === "https:" ? "wss:" : "ws:";
    const proxyWsUrl = `${wsProto}//${window.location.host}/api/ws/hosts/${hostId}/incus-proxy${incusPath}`;

    const callerProtocols =
      typeof protocols === "string"
        ? [protocols]
        : Array.isArray(protocols)
          ? [...protocols]
          : [];

    const mergedProtocols = [...callerProtocols];
    if (cachedTicket) {
      mergedProtocols.push(`${SUBPROTOCOL_PREFIX}${cachedTicket}`);
    }

    return mergedProtocols.length > 0
      ? new NativeWebSocket(proxyWsUrl, mergedProtocols)
      : new NativeWebSocket(proxyWsUrl);
  } as unknown as typeof WebSocket;

  PatchedWebSocket.prototype = NativeWebSocket.prototype;
  Object.defineProperties(PatchedWebSocket, {
    CONNECTING: { value: NativeWebSocket.CONNECTING },
    OPEN: { value: NativeWebSocket.OPEN },
    CLOSING: { value: NativeWebSocket.CLOSING },
    CLOSED: { value: NativeWebSocket.CLOSED },
  });
  window.WebSocket = PatchedWebSocket;

  // 4. Intercept native <a href="/.../1.0/..."> download links
  document.addEventListener(
    "click",
    (event) => {
      const target = event.target as HTMLElement | null;
      const anchor = target?.closest?.("a[href]") as HTMLAnchorElement | null;
      if (!anchor) return;
      const rawHref = anchor.getAttribute("href");
      if (!rawHref) return;

      const incusPath = extractIncusApiPath(rawHref, rootPath);
      if (!incusPath || !cachedTicket) return;

      const separator = incusPath.includes("?") ? "&" : "?";
      anchor.href = `/api/admin/hosts/${hostId}/incus-proxy${incusPath}${separator}__incudal_ticket=${encodeURIComponent(cachedTicket)}`;
    },
    true,
  );
}
