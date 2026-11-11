// All protected queries report session expiry to the shared login boundary.
export async function apiFetch(input: RequestInfo | URL, init?: RequestInit) {
  const response = await fetch(input, init);
  if (response.status === 401)
    window.dispatchEvent(new Event("node-explorer:unauthenticated"));
  return response;
}
