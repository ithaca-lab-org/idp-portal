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

/** The secret naming rules, served by the API so the page checks exactly what the API checks. */
export interface NameRules {
  reservedNames: string[];
  reservedPrefixes: string[];
}

export interface SecretsInfo extends NameRules {
  namespaces: Namespace[];
}

/** The namespaces the user may manage secrets in; null when the Secrets page is turned off (404). */
export async function listNamespaces(): Promise<SecretsInfo | null> {
  try {
    return await call<SecretsInfo>("/api/namespaces");
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

const seg = encodeURIComponent;

export const listSecrets = async (ns: string) =>
  (await call<{ secrets: SecretMeta[] }>(`/api/namespaces/${seg(ns)}/secrets`)).secrets;

/**
 * Replaces the whole secret. With create, the server answers 409 if the secret already exists.
 * The response never contains values.
 */
export const putSecret = (ns: string, name: string, data: Record<string, string>, create = false) =>
  call<SecretResult>(`/api/namespaces/${seg(ns)}/secrets/${seg(name)}${create ? "?create=true" : ""}`, {
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

export interface WorkloadCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

/** A WorkloadIdentity as the operator reports it; spiffeId and hostname are empty until it has. */
export interface Workload {
  namespace: string;
  name: string;
  serviceAccount: string;
  spiffeId: string;
  hostname: string;
  status: Status;
  reason?: string;
  conditions: WorkloadCondition[];
  createdAt?: string;
}

/** The namespaces the user may order workloads in; null when the Workloads page is turned off (404). */
export async function listWorkloadNamespaces(): Promise<SecretsInfo | null> {
  try {
    return await call<SecretsInfo>("/api/workloads/namespaces");
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

export const listWorkloads = async (ns: string) =>
  (await call<{ workloads: Workload[] }>(`/api/workloads?namespace=${seg(ns)}`)).workloads;

export const getWorkload = (ns: string, name: string) => call<Workload>(`/api/workloads/${seg(ns)}/${seg(name)}`);

export const placeWorkload = (namespace: string, name: string) =>
  call<Workload>("/api/orders/workloads", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ namespace, name }),
  });

export interface AccessOption {
  value: "reader" | "writer";
  label: string;
}

/** What the Resources page needs up front: the namespaces and the rules the API applies. */
export interface ResourcesInfo {
  namespaces: Namespace[];
  maxNamespaceLength: number;
  maxNameLength: number;
  maxBuckets: number;
  maxGrants: number;
  access: AccessOption[];
}

/** A BucketClaim as the broker reports it; message is the portal's text, never the cloud's. */
export interface Bucket {
  namespace: string;
  name: string;
  bucketName: string;
  status: Status;
  reason?: string;
  message?: string;
  createdAt?: string;
}

export interface Grant {
  namespace: string;
  name: string;
  workload: string;
  bucket: string;
  access: "reader" | "writer";
  status: Status;
  reason?: string;
  message?: string;
  createdAt?: string;
}

export interface BucketDetail extends Bucket {
  grants: Grant[];
}

/** The namespaces the user may order buckets in; null when the Resources page is turned off (404). */
export async function listResourceNamespaces(): Promise<ResourcesInfo | null> {
  try {
    return await call<ResourcesInfo>("/api/resources/namespaces");
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

export const listBuckets = async (ns: string) =>
  (await call<{ buckets: Bucket[] }>(`/api/buckets?namespace=${seg(ns)}`)).buckets;

export const getBucket = (ns: string, name: string) => call<BucketDetail>(`/api/buckets/${seg(ns)}/${seg(name)}`);

export const placeBucket = (namespace: string, name: string) =>
  call<Bucket>("/api/orders/buckets", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ namespace, name }),
  });

export const placeGrant = (namespace: string, workload: string, bucket: string, access: string) =>
  call<Grant>("/api/orders/grants", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ namespace, workload, bucket, access }),
  });

/** Revokes a grant (204). There is no way to delete a bucket from the portal. */
export async function revokeGrant(ns: string, name: string): Promise<void> {
  const res = await fetch(`/api/grants/${seg(ns)}/${seg(name)}`, { method: "DELETE", credentials: "same-origin" });
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
