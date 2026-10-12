export async function copyText(text: string, source: HTMLElement) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Permissions may block the modern API even on HTTPS.
    }
  }

  const focused = document.activeElement;
  const selection = document.getSelection();
  const ranges = selection
    ? Array.from({ length: selection.rangeCount }, (_, i) =>
        selection.getRangeAt(i).cloneRange(),
      )
    : [];
  const input = document.createElement("textarea");
  input.value = text;
  input.readOnly = true;
  input.tabIndex = -1;
  input.style.cssText =
    "position:fixed;top:0;left:0;opacity:0;pointer-events:none;font-size:16px";
  // Keep the input inside the dialog so its focus trap allows selection.
  (source.closest('[role="dialog"]') || document.body).append(input);
  try {
    input.focus({ preventScroll: true });
    input.select();
    input.setSelectionRange(0, input.value.length);
    if (!document.execCommand("copy")) {
      throw new Error("Clipboard copy failed");
    }
  } finally {
    input.remove();
    if (focused instanceof HTMLElement) {
      focused.focus({ preventScroll: true });
    }
    if (selection) {
      selection.removeAllRanges();
      ranges.forEach((range) => selection.addRange(range));
    }
  }
}
