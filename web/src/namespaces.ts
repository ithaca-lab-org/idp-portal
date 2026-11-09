// The namespace picked on the Secrets, Workloads and Resources pages is shared and kept for the browser session.
const KEY = "idp.ns";

export function rememberedNs(): string {
  try {
    return sessionStorage.getItem(KEY) ?? "";
  } catch {
    return "";
  }
}

export function rememberNs(ns: string) {
  try {
    sessionStorage.setItem(KEY, ns);
  } catch {
    // storage may be blocked: the choice then lasts until the page is reloaded
  }
}

/** The wanted namespace if the page can use it, else the first usable one. */
export function pickNs(usable: string[], wanted: string): string {
  return usable.includes(wanted) ? wanted : (usable[0] ?? "");
}
