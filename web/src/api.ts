export type Status = "PENDING" | "COMPLETE" | "FAILED" | "RELEASED";

export interface User {
  sub: string;
  username: string;
  email: string;
  groups: string[];
}

export interface Order {
  id: string;
  kind: string;
  name: string;
  businessUnit: string;
  owner: string;
  size: string;
  status: Status;
  message?: string;
  createdAt: string;
  updatedAt: string;
}

export interface NamespaceOrder {
  name: string;
  businessUnit: string;
  size: "small" | "medium";
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: "same-origin", ...init });
  let body: unknown = null;
  try {
    body = await res.json();
  } catch {
    /* non-JSON body: fall through with a generic message */
  }
  if (!res.ok) {
    const msg = (body as { error?: string } | null)?.error ?? `request failed (${res.status})`;
    throw new ApiError(res.status, msg);
  }
  return body as T;
}

/** Returns null when nobody is signed in. */
export async function me(): Promise<User | null> {
  try {
    return await call<User>("/auth/me");
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return null;
    throw e;
  }
}

export const businessUnits = async () =>
  (await call<{ businessUnits: string[] }>("/api/business-units")).businessUnits;

export const listOrders = async () => (await call<{ orders: Order[] }>("/api/orders")).orders;

export const placeOrder = (o: NamespaceOrder) =>
  call<Order>("/api/orders/namespaces", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(o),
  });

/** Ends the local session; returns the Keycloak end-session URL to visit next, if any. */
export const logout = async () =>
  (await call<{ end_session_url?: string }>("/auth/logout", { method: "POST" })).end_session_url;

export interface Namespace {
  name: string;
  businessUnit: string;
}

export interface SecretMeta {
  name: string;
  keys: string[];
  version: number;
  updatedBy: string;
  updatedAt: string;
}

export interface SecretResult {
  name: string;
  keys: string[];
  version: number;
}

/** The namespaces the user may manage secrets in; null when the Secrets page is turned off (404). */
export async function listNamespaces(): Promise<Namespace[] | null> {
  try {
    return (await call<{ namespaces: Namespace[] }>("/api/namespaces")).namespaces;
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

const seg = encodeURIComponent;

export const listSecrets = async (ns: string) =>
  (await call<{ secrets: SecretMeta[] }>(`/api/namespaces/${seg(ns)}/secrets`)).secrets;

/** Replaces the whole secret. The response never contains values. */
export const putSecret = (ns: string, name: string, data: Record<string, string>) =>
  call<SecretResult>(`/api/namespaces/${seg(ns)}/secrets/${seg(name)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ data }),
  });

export async function deleteSecret(ns: string, name: string): Promise<void> {
  const res = await fetch(`/api/namespaces/${seg(ns)}/secrets/${seg(name)}`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) {
    let msg = `request failed (${res.status})`;
    try {
      msg = ((await res.json()) as { error?: string }).error ?? msg;
    } catch {
      /* keep the generic message */
    }
    throw new ApiError(res.status, msg);
  }
}
