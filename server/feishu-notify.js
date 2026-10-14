// Feishu custom bot webhook notifier. Posts plain-text alerts and collapses
// repeated identical messages while a throttle window is still open, so a
// prolonged outage cannot flood the chat.
export function createFeishuNotifier({
  webhook,
  fetchImpl = fetch,
  throttleMs = 30 * 60_000,
  now = () => Date.now(),
} = {}) {
  if (!webhook) return async () => false;
  let lastSentAt = -Infinity;
  let lastMessage = "";
  return async function notify(message) {
    if (!message) return false;
    if (message === lastMessage && now() - lastSentAt < throttleMs) return false;
    try {
      const response = await fetchImpl(webhook, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ msg_type: "text", content: { text: message } }),
        signal: AbortSignal.timeout(10_000),
      });
      const payload = await response.json().catch(() => null);
      const ok =
        response.ok && (payload?.code === 0 || payload?.StatusCode === 0);
      if (ok) {
        lastSentAt = now();
        lastMessage = message;
      } else {
        console.error(
          `Feishu notify rejected (HTTP ${response.status}): ${payload?.msg || payload?.StatusMessage || "unknown error"}`,
        );
      }
      return ok;
    } catch (error) {
      console.error(`Feishu notify failed: ${error.message}`);
      return false;
    }
  };
}
